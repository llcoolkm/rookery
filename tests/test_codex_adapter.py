import subprocess
import unittest
from unittest.mock import MagicMock, patch
from rookery import codex_adapter as adapter

ID = '0199ee11-59a2-766b-9911-f86af98f6729'

class AdapterTests(unittest.TestCase):
    def test_state_is_fail_closed(self):
        for state in [{'type':'notLoaded'}, {'type':'systemError'}, {}, {'type':'newUnknownState'}]:
            self.assertEqual(adapter.normalize({'id':ID,'status':state})['status'], 'unknown')
        self.assertEqual(adapter.normalize({'id':ID,'status':{'type':'idle'}})['status'], 'unknown')
        self.assertEqual(adapter.normalize({'id':ID,'status':{'type':'idle'},'canAcceptDirectInput':True})['status'], 'idle')
        # A failed turn leaves systemError while the thread still takes input.
        self.assertEqual(adapter.normalize({'id':ID,'status':{'type':'systemError'},'canAcceptDirectInput':True})['status'], 'idle')
        for flag in ['waitingOnApproval','waitingOnUserInput','newUnknownFlag']:
            self.assertEqual(adapter.normalize({'id':ID,'status':{'type':'active','activeFlags':[flag]}})['status'],'waiting')

    @patch.object(adapter.subprocess, 'run')
    @patch.object(adapter, 'Proxy')
    def test_waiting_is_never_submitted(self, proxy, run):
        proxy.return_value.__enter__.return_value.call.return_value = {'thread':{'id':ID,'status':{'type':'active','activeFlags':['waitingOnApproval']}}}
        self.assertEqual(adapter.submit({'id':ID}, 'payload')['status'], 'held')
        run.assert_not_called()

    @patch.object(adapter.subprocess, 'run')
    @patch.object(adapter, 'Proxy')
    def test_submission_never_claims_receipt_and_no_shell(self, proxy, run):
        proxy.return_value.__enter__.return_value.call.return_value = {'thread':{'id':ID,'status':{'type':'idle'},'canAcceptDirectInput':True}}
        run.return_value = subprocess.CompletedProcess([], 0)
        result = adapter.submit({'id':ID}, 'untrusted $(touch /tmp/should-not-exist)')
        self.assertEqual(result['status'], 'submitted')
        self.assertIn('unconfirmed', result['detail'])
        args, kwargs = run.call_args
        self.assertEqual(args[0], ['codex','queue','--thread',ID,'--message','untrusted $(touch /tmp/should-not-exist)'])
        self.assertNotIn('shell', kwargs)

    @patch.object(adapter.subprocess, 'run')
    @patch.object(adapter, 'Proxy')
    def test_timeout_not_retryable_failure(self, proxy, run):
        proxy.return_value.__enter__.return_value.call.return_value = {'thread':{'id':ID,'status':{'type':'idle'},'canAcceptDirectInput':True}}
        run.side_effect = subprocess.TimeoutExpired('codex',20)
        self.assertEqual(adapter.submit({'id':ID}, 'message')['status'], 'uncertain')

    @patch.object(adapter, 'Proxy')
    def test_non_uuid_never_contacts_provider(self, proxy):
        self.assertEqual(adapter.submit({'id':'ambiguous name'}, 'message')['status'], 'failed')
        proxy.assert_not_called()

    @patch.object(adapter, 'Proxy')
    def test_discovery_uses_loaded_runtime_ids_not_history(self, proxy):
        rpc = proxy.return_value.__enter__.return_value.call
        rpc.side_effect = [
            {'data': [ID], 'nextCursor': None},
            {'thread': {'id': ID, 'name': 'live-only', 'status': {'type': 'idle'}, 'canAcceptDirectInput': True}},
        ]
        sessions = adapter.discover()
        self.assertEqual(sessions[0]['name'], 'live-only')
        self.assertEqual(sessions[0]['status'], 'idle')
        self.assertEqual([c.args[0] for c in rpc.call_args_list], ['thread/loaded/list', 'thread/read'])

    def test_rpc_notifications_do_not_steal_response(self):
        import json
        import types
        connection = MagicMock()
        connection.recv.side_effect = [
            json.dumps({'method':'approval/request','id':1,'params':{}}),
            json.dumps({'id':1,'result':{'ok':'initialize'}}),
            json.dumps({'method':'approval/request','id':2,'params':{}}),
            json.dumps({'id':2,'result':{'ok':'thread/list'}}),
        ]
        connect = MagicMock(return_value=connection)
        with patch('rookery.local_ws.LocalWebSocket', connect):
            with adapter.Proxy(socket_path='/test/socket') as proxy:
                self.assertEqual(proxy.call('thread/list',{})['ok'],'thread/list')
        sent = [json.loads(call.args[0]) for call in connection.send.call_args_list]
        self.assertEqual([item['method'] for item in sent],['initialize','initialized','thread/list'])
        connect.assert_called_once_with('/test/socket',timeout=8.0)
        connection.close.assert_called_once()

if __name__ == '__main__':
    unittest.main()
