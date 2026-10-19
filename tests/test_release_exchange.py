"""One real hub and two real helpers: store/forward, restart and reply."""
import tempfile
from pathlib import Path
import unittest
import uuid

from rookery.client import Client, configure
from rookery.daemon import Helper
from rookery.hub import Hub


class NodeClient:
    def __init__(self, hub, node):
        self.hub,self.node = hub,node

    def call(self, op, data=None):
        return self.hub.call(self.node,op,data or {})


class ReleaseExchangeTests(unittest.TestCase):
    def test_buffered_exchange_and_reply_survive_helpers_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            hub = Hub(base/'hub'/'hub.sqlite3')
            self.addCleanup(hub.close)
            sessions = {node:{'provider':provider,'id':str(uuid.uuid4()),'name':node,'status':'idle'}
                        for node,provider in [('server','codex'),('laptop','claude')]}
            addresses = {n:f"{n}/{s['provider']}:{s['id']}" for n,s in sessions.items()}
            calls = []
            helpers = {}
            for node in sessions:
                configure(base/node,node,'http://127.0.0.1:9999','x'*40,True)
                helpers[node] = Helper(base/node,client=NodeClient(hub,node),
                    discover=lambda n=node:([dict(sessions[n])],{}),
                    submit=lambda s,t:(calls.append((s['id'],t)) or {'status':'submitted','detail':'test harness accepted'}))
                helpers[node].publish()
                hub.call(node,'enroll',{'target':addresses[node],'alias':'reviewer' if node=='laptop' else 'coordinator'})
            mid = str(uuid.uuid4())
            client = Client(base/'server',addresses['server'])
            client.request('send',{'actor':addresses['server'],'to':'laptop/reviewer','id':mid,'body':'Review nonce AB12','ttl':3600},timeout=0)
            # Sender helper uploads; recipient remains stopped and hub stores it.
            helpers['server'].requests()
            self.assertEqual(hub.call('server','status')['messages'][0]['status'],'pending')
            helpers['laptop'].sync_once()
            helpers['laptop'].cycle()
            self.assertEqual(len(calls),1)
            self.assertIn('Review nonce AB12',calls[0][1])
            # Recreate the recipient helper against existing on-disk state.
            restarted = Helper(base/'laptop',client=NodeClient(hub,'laptop'),
                discover=lambda:([dict(sessions['laptop'])],{}),
                submit=lambda s,t:(calls.append((s['id'],t)) or {'status':'submitted'}))
            restarted.store.recover_attempts()
            restarted.sync_once()
            restarted.cycle()
            self.assertEqual(len(calls),1)
            reply = str(uuid.uuid4())
            Client(base/'laptop',addresses['laptop']).request('reply',{'actor':addresses['laptop'],
                'message_id':mid,'id':reply,'body':'AB12 reviewed','ttl':3600},timeout=0)
            restarted.requests()
            helpers['server'].sync_once()
            helpers['server'].cycle()
            self.assertEqual(len(calls),2)
            self.assertIn('AB12 reviewed',calls[1][1])
            self.assertEqual(client.status(mid)['status'],'replied')


if __name__ == '__main__':
    unittest.main()
