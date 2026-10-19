"""Bounded, synchronous WebSocket text transport over local Unix sockets only.

This deliberately supports no TCP, TLS, HTTP proxy, extensions or compression.
It connects to the existing Codex daemon; it never starts or configures one.
"""
from __future__ import annotations

import base64
import hashlib
import os
import socket
import struct
import time

MAX_MESSAGE = 16 * 1024 * 1024
MAX_HEADERS = 16 * 1024
_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11'


class LocalWebSocketError(RuntimeError):
    pass


class LocalWebSocket:
    def __init__(self, path: str, timeout: float = 8.0):
        if timeout <= 0:
            raise ValueError('timeout must be positive')
        self.timeout = timeout
        self.buffer = bytearray()
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.closed = False
        deadline = time.monotonic() + timeout
        try:
            self._remaining(deadline)
            self.sock.connect(path)
            self._handshake(deadline)
        except Exception:
            self.close()
            raise

    def _remaining(self, deadline: float) -> None:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError('Local WebSocket operation timed out')
        self.sock.settimeout(remaining)

    def _exact(self, count: int, deadline: float) -> bytes:
        self._remaining(deadline)
        while len(self.buffer) < count:
            self._remaining(deadline)
            part = self.sock.recv(min(65536, count - len(self.buffer)))
            if not part:
                raise LocalWebSocketError('Local WebSocket connection closed')
            self.buffer.extend(part)
        result = bytes(self.buffer[:count])
        del self.buffer[:count]
        return result

    def _handshake(self, deadline: float) -> None:
        key = base64.b64encode(os.urandom(16)).decode('ascii')
        request = ('GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n'
                   'Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n'
                   f'Sec-WebSocket-Key: {key}\r\n\r\n')
        self._remaining(deadline)
        self.sock.sendall(request.encode('ascii'))
        while b'\r\n\r\n' not in self.buffer:
            if len(self.buffer) >= MAX_HEADERS:
                raise LocalWebSocketError('WebSocket upgrade headers too large')
            self._remaining(deadline)
            part = self.sock.recv(min(4096, MAX_HEADERS - len(self.buffer)))
            if not part:
                raise LocalWebSocketError('Connection closed during WebSocket upgrade')
            self.buffer.extend(part)
        end = self.buffer.index(b'\r\n\r\n')
        head = bytes(self.buffer[:end])
        del self.buffer[:end + 4]
        lines = head.decode('iso-8859-1').split('\r\n')
        status = lines.pop(0).split(' ', 2)
        if len(status) < 2 or status[:2] != ['HTTP/1.1', '101']:
            raise LocalWebSocketError('WebSocket HTTP upgrade refused')
        headers: dict[str, str] = {}
        for line in lines:
            if ':' not in line or line[:1].isspace():
                raise LocalWebSocketError('Invalid WebSocket upgrade header')
            name, value = line.split(':', 1)
            name = name.lower().strip()
            if name in headers:
                raise LocalWebSocketError('Duplicate WebSocket upgrade header')
            headers[name] = value.strip()
        expected = base64.b64encode(hashlib.sha1((key + _GUID).encode('ascii')).digest()).decode('ascii')
        if (headers.get('upgrade', '').lower() != 'websocket'
                or 'upgrade' not in {token.strip().lower() for token in headers.get('connection', '').split(',')}
                or headers.get('sec-websocket-accept') != expected):
            raise LocalWebSocketError('Invalid WebSocket upgrade response')
        if 'sec-websocket-extensions' in headers or 'sec-websocket-protocol' in headers:
            raise LocalWebSocketError('Unrequested WebSocket extension or subprotocol')

    def _send_frame(self, opcode: int, payload: bytes, deadline: float) -> None:
        if self.closed:
            raise LocalWebSocketError('Local WebSocket is closed')
        length = len(payload)
        mask = os.urandom(4)
        if length < 126:
            header = struct.pack('!BB', 0x80 | opcode, 0x80 | length)
        elif length < 65536:
            header = struct.pack('!BBH', 0x80 | opcode, 0xFE, length)
        else:
            header = struct.pack('!BBQ', 0x80 | opcode, 0xFF, length)
        masked = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
        self._remaining(deadline)
        self.sock.sendall(header + mask + masked)

    def send(self, text: str) -> None:
        payload = text.encode('utf-8')
        if len(payload) > MAX_MESSAGE:
            raise LocalWebSocketError('WebSocket message exceeds 16 MiB')
        try:
            self._send_frame(1, payload, time.monotonic() + self.timeout)
        except Exception:
            self.close()
            raise

    def recv(self, timeout: float | None = None) -> str:
        deadline = time.monotonic() + (self.timeout if timeout is None else timeout)
        parts = bytearray()
        fragmented = False
        try:
            while True:
                first, second = self._exact(2, deadline)
                fin, opcode = bool(first & 0x80), first & 0x0F
                if first & 0x70 or second & 0x80:
                    raise LocalWebSocketError('Unsupported RSV bits or masked server frame')
                if opcode not in (0, 1, 8, 9, 10):
                    raise LocalWebSocketError('Unsupported WebSocket frame opcode')
                marker = second & 0x7F
                if opcode >= 8 and (not fin or marker > 125):
                    raise LocalWebSocketError('Invalid WebSocket control frame')
                length = marker
                if marker == 126:
                    length = struct.unpack('!H', self._exact(2, deadline))[0]
                    if length < 126:
                        raise LocalWebSocketError('Noncanonical WebSocket frame length')
                elif marker == 127:
                    length = struct.unpack('!Q', self._exact(8, deadline))[0]
                    if length < 65536 or length & (1 << 63):
                        raise LocalWebSocketError('Invalid WebSocket frame length')
                if length > MAX_MESSAGE or (opcode < 8 and len(parts) + length > MAX_MESSAGE):
                    raise LocalWebSocketError('WebSocket message exceeds 16 MiB')
                if opcode == 0 and not fragmented:
                    raise LocalWebSocketError('Unexpected WebSocket continuation')
                if opcode == 1 and fragmented:
                    raise LocalWebSocketError('New text frame during fragmented message')
                payload = self._exact(length, deadline)
                if opcode == 8:
                    raise LocalWebSocketError('Local WebSocket peer closed connection')
                if opcode == 9:
                    self._send_frame(10, payload, deadline)
                    continue
                if opcode == 10:
                    continue
                parts.extend(payload)
                if fin:
                    try:
                        return parts.decode('utf-8')
                    except UnicodeDecodeError as exc:
                        raise LocalWebSocketError('Invalid UTF-8 WebSocket text') from exc
                fragmented = True
        except Exception:
            self.close()
            raise

    def close(self) -> None:
        if not self.closed:
            self.closed = True
            self.sock.close()
