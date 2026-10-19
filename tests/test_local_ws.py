import base64
import hashlib
import struct
import unittest
from unittest.mock import patch

from rookery.local_ws import LocalWebSocket, LocalWebSocketError, MAX_MESSAGE

KEY = base64.b64encode(b'k' * 16).decode()
ACCEPT = base64.b64encode(hashlib.sha1((KEY + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
UPGRADE = b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: ' + ACCEPT + b'\r\n\r\n'


def frame(payload=b'hello', first=0x81):
    size = len(payload)
    header = bytes([first, size]) if size < 126 else bytes([first, 126]) + struct.pack('!H', size)
    return header + payload


class FakeSocket:
    def __init__(self, data=UPGRADE, partial=None):
        self.data = bytearray(data)
        self.partial = partial
        self.sent = []
        self.closed = False

    def settimeout(self, value):
        self.timeout = value

    def connect(self, path):
        self.path = path

    def recv(self, size):
        if self.partial:
            size = min(size, self.partial)
        result = bytes(self.data[:size])
        del self.data[:size]
        return result

    def sendall(self, data):
        self.sent.append(data)

    def close(self):
        self.closed = True


class LocalWebSocketTests(unittest.TestCase):
    def make(self, data=UPGRADE, partial=None):
        sock = FakeSocket(data, partial)
        with patch('rookery.local_ws.socket.socket', return_value=sock), patch('rookery.local_ws.os.urandom', side_effect=lambda n: b'k' * n):
            client = LocalWebSocket('/local/socket')
        return client, sock

    def test_valid_handshake_and_partial_reads(self):
        client, sock = self.make(UPGRADE + frame('héllo'.encode()), partial=1)
        self.assertEqual(client.recv(), 'héllo')
        self.assertEqual(sock.path, '/local/socket')
        client.close()
        self.assertTrue(sock.closed)

    def test_handshake_accept_and_headers_checked(self):
        for bad in [UPGRADE.replace(ACCEPT, b'wrong'), UPGRADE.replace(b'101', b'200'),
                    UPGRADE.replace(b'Upgrade: websocket', b'Upgrade: other'),
                    UPGRADE.replace(b'keep-alive, Upgrade', b'keep-alive'),
                    UPGRADE.replace(b'\r\n\r\n', b'\r\nSec-WebSocket-Extensions: deflate\r\n\r\n')]:
            sock = FakeSocket(bad)
            with patch('rookery.local_ws.socket.socket', return_value=sock), patch('rookery.local_ws.os.urandom', return_value=b'k'*16):
                with self.assertRaises(LocalWebSocketError):
                    LocalWebSocket('/local/socket')
                self.assertTrue(sock.closed)

    def test_oversized_handshake(self):
        with self.assertRaises(LocalWebSocketError):
            self.make(b'H' * (16*1024 + 1))

    def test_connect_failure_closes_socket(self):
        sock = FakeSocket()
        with patch('rookery.local_ws.socket.socket', return_value=sock), patch.object(sock,'connect',side_effect=OSError('denied')):
            with self.assertRaises(OSError):
                LocalWebSocket('/local/socket')
        self.assertTrue(sock.closed)

    def test_fragmented_utf8_with_interleaved_ping(self):
        client, sock = self.make(UPGRADE + frame(b'\xc3',0x01) + frame(b'ping',0x89) + frame(b'\xa9',0x80))
        self.assertEqual(client.recv(), 'é')
        pong = sock.sent[-1]
        self.assertEqual(pong[0], 0x8A)
        self.assertTrue(pong[1] & 0x80)
        mask, payload = pong[2:6], pong[6:]
        self.assertEqual(bytes(b ^ mask[i%4] for i,b in enumerate(payload)),b'ping')

    def test_reject_unsupported_and_malformed_frames(self):
        bad_frames = [frame(b'x',0xC1), frame(b'x',0x82), frame(b'x',0x80),
                      frame(b'x',0x09), b'\x89\x7e', b'\x81\x81',
                      frame(b'x',0x01)+frame(b'y',0x81), frame(b'\xff'),
                      b'\x81\x7e\x00\x01', b'\x81\x7f'+struct.pack('!Q',MAX_MESSAGE+1)]
        for bad in bad_frames:
            with self.subTest(frame=bad):
                client, sock = self.make(UPGRADE+bad)
                with self.assertRaises(LocalWebSocketError):
                    client.recv()
                self.assertTrue(sock.closed)

    def test_total_fragment_size_cap(self):
        client, sock = self.make(UPGRADE+frame(b'abc',0x01)+frame(b'def',0x80))
        with patch('rookery.local_ws.MAX_MESSAGE',5):
            with self.assertRaises(LocalWebSocketError):
                client.recv()
        self.assertTrue(sock.closed)

    def test_ping_does_not_extend_deadline(self):
        client, sock = self.make(UPGRADE+frame(b'p',0x89)*100)
        # All frames are already buffered; deadline must still be checked.
        ticks = iter(range(100))
        with patch('rookery.local_ws.time.monotonic',side_effect=lambda: next(ticks)):
            with self.assertRaises(TimeoutError):
                client.recv(timeout=4)
        self.assertTrue(sock.closed)

    def test_send_is_masked_and_bounded(self):
        client, sock = self.make()
        client.send('hello')
        payload = sock.sent[-1]
        self.assertEqual(payload[:2], b'\x81\x85')
        mask = payload[2:6]
        self.assertEqual(bytes(b ^ mask[i%4] for i,b in enumerate(payload[6:])),b'hello')
        with patch('rookery.local_ws.MAX_MESSAGE',4):
            with self.assertRaises(LocalWebSocketError):
                client.send('hello')

    def test_truncated_payload_closes_connection(self):
        client, sock = self.make(UPGRADE+b'\x81\x05ab')
        with self.assertRaises(LocalWebSocketError):
            client.recv()
        self.assertTrue(sock.closed)

class LocalWebSocketBoundaryTests(unittest.TestCase):
    """Additional frame boundaries and failures, using the same bounded fake I/O."""

    make = LocalWebSocketTests.make

    def test_extended_length_send_and_receive(self):
        for length in (125, 126, 65535, 65536):
            with self.subTest(length=length):
                data = b'x' * length
                if length < 126:
                    header = bytes([0x81, length])
                elif length < 65536:
                    header = b'\x81\x7e' + struct.pack('!H', length)
                else:
                    header = b'\x81\x7f' + struct.pack('!Q', length)
                client, sock = self.make(UPGRADE + header + data, partial=257)
                self.assertEqual(client.recv(), data.decode())
                client.send(data.decode())
                sent = sock.sent[-1]
                self.assertEqual(sent[0], 0x81)
                marker = sent[1] & 0x7f
                self.assertTrue(sent[1] & 0x80)
                offset = 2
                wire_length = marker
                if marker == 126:
                    wire_length = struct.unpack('!H', sent[2:4])[0]
                    offset = 4
                elif marker == 127:
                    wire_length = struct.unpack('!Q', sent[2:10])[0]
                    offset = 10
                self.assertEqual(wire_length, length)
                mask, payload = sent[offset:offset+4], sent[offset+4:]
                self.assertEqual(bytes(b ^ mask[i % 4] for i, b in enumerate(payload)), data)

    def test_send_failure_closes_connection(self):
        client, sock = self.make()
        with patch.object(sock, 'sendall', side_effect=BrokenPipeError('peer gone')):
            with self.assertRaises(BrokenPipeError):
                client.send('question')
        self.assertTrue(sock.closed)
        with self.assertRaises(LocalWebSocketError):
            client.send('retry must reconnect')

    def test_slow_partial_payload_cannot_extend_deadline(self):
        client, sock = self.make()
        sock.data.extend(b'\x81\x08abcdefgh')
        sock.partial = 1
        ticks = iter(range(100))
        with patch('rookery.local_ws.time.monotonic', side_effect=lambda: next(ticks)):
            with self.assertRaises(TimeoutError):
                client.recv(timeout=5)
        self.assertTrue(sock.closed)

    def test_multiple_buffered_messages_are_not_lost(self):
        client, _ = self.make(UPGRADE + frame(b'first') + frame(b'second'))
        self.assertEqual(client.recv(), 'first')
        self.assertEqual(client.recv(), 'second')

    def test_empty_text_and_fragmented_message(self):
        client, _ = self.make(UPGRADE + frame(b'') + frame(b'', 0x01) + frame(b'', 0x80))
        self.assertEqual(client.recv(), '')
        self.assertEqual(client.recv(), '')


if __name__ == '__main__':
    unittest.main()
