import json
import os
from pathlib import Path
import tempfile
import unittest
import uuid
from unittest.mock import patch

from rookery import polling
from rookery.core import Error
from rookery.local_store import Store


def process_stat(pid=123,state='S',start='123456',comm='codex worker (a) ) child'):
    fields=[state]+['0']*18+[start]+['0']*8
    return f'{pid} ({comm}) '+' '.join(fields)+'\n'


class PollingTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        self.store=Store(self.root/'state')
        self.proc=self.root/'proc'
        (self.proc/'123').mkdir(parents=True)
        (self.proc/'123/stat').write_text(process_stat())
        (self.proc/'sys/kernel/random').mkdir(parents=True)
        self.boot=str(uuid.uuid4())
        (self.proc/'sys/kernel/random/boot_id').write_text(self.boot+'\n')
        patcher=patch.object(polling,'PROC',self.proc);patcher.start();self.addCleanup(patcher.stop)
        self.sid=str(uuid.uuid4())

    def register(self):return polling.register(self.store,'codex',self.sid,123,'Laptop Codex')

    def record(self):return self.store.path/'network/polling'/f'codex-{self.sid}.json'

    def test_stat_comm_parentheses_and_spaces(self):
        self.assertEqual(polling.parse_stat(process_stat()),('S','123456'))
        for bad in ['broken', '1 (name) S 0', '1 name) S '+'0 '*30]:
            with self.assertRaises(Error):polling.parse_stat(bad)

    def test_registered_live_session_is_never_native_idle(self):
        self.register()
        self.assertEqual(polling.discover(self.store),[{'provider':'codex','id':self.sid,
            'pid':123,'name':'Laptop Codex','status':'waiting','delivery':'polling'}])
        saved=json.loads(self.record().read_text())
        self.assertEqual(saved['starttime'],'123456');self.assertEqual(saved['boot_id'],self.boot)
        self.assertEqual(self.record().stat().st_mode&0o777,0o600)
        self.assertEqual(self.record().parent.stat().st_mode&0o777,0o700)

    def test_agy_process_can_only_register_for_polling(self):
        polling.register(self.store, 'agy', self.sid, 123, 'Antigravity CLI')
        self.assertEqual(polling.discover(self.store), [{'provider':'agy','id':self.sid,
            'pid':123,'name':'Antigravity CLI','status':'waiting','delivery':'polling'}])

    def test_pid_reuse_omitted(self):
        self.register()
        (self.proc/'123/stat').write_text(process_stat(start='999999'))
        self.assertEqual(polling.discover(self.store),[])

    def test_reboot_omitted(self):
        self.register()
        (self.proc/'sys/kernel/random/boot_id').write_text(str(uuid.uuid4()))
        self.assertEqual(polling.discover(self.store),[])

    def test_dead_and_stopped_processes_cannot_register_or_discover(self):
        self.register()
        for state in ['Z','X','x','T','t','?']:
            (self.proc/'123/stat').write_text(process_stat(state=state))
            self.assertEqual(polling.discover(self.store),[])
            with self.assertRaises(Error):self.register()
        (self.proc/'123/stat').unlink()
        self.assertEqual(polling.discover(self.store),[])

    def test_foreign_owner_rejected(self):
        uid=os.getuid()
        with patch.object(polling.os,'getuid',return_value=uid+1):
            with self.assertRaisesRegex(Error,'owned'):
                polling._pin(123)

    def test_invalid_provider_uuid_pid_rejected(self):
        for provider,sid,pid in [('other',self.sid,123),('codex','../escape',123),('codex',self.sid.upper(),123),
                                 ('codex',self.sid,True),('codex',self.sid,0),('codex',self.sid,-1),('codex',self.sid,'123')]:
            with self.subTest(provider=provider,sid=sid,pid=pid):
                with self.assertRaises(Error):polling.register(self.store,provider,sid,pid)

    def test_unregister_only_exact_identity_and_is_idempotent(self):
        self.register()
        other=str(uuid.uuid4());polling.register(self.store,'claude',other,123)
        polling.unregister(self.store,'codex',self.sid)
        polling.unregister(self.store,'codex',self.sid)
        live=polling.discover(self.store)
        self.assertEqual([s['id'] for s in live],[other])

    def test_corrupt_records_skipped_without_hiding_valid_peer(self):
        self.register();directory=self.record().parent
        for name,data in [('bad.json','{torn'),('wrong.json',self.record().read_text()),('huge.json','x'*5000),('array.json','[]')]:
            path=directory/name;path.write_text(data);path.chmod(0o600)
        self.assertEqual(len(polling.discover(self.store)),1)
        saved=json.loads(self.record().read_text());saved['pid']='123';self.record().write_text(json.dumps(saved))
        self.assertEqual(polling.discover(self.store),[])

    def test_symlink_record_and_public_record_fail_closed(self):
        self.register();saved=self.record().read_text();self.record().unlink()
        target=self.root/'outside.json';target.write_text(saved);target.chmod(0o600)
        self.record().symlink_to(target)
        self.assertEqual(polling.discover(self.store),[])
        self.record().unlink();self.record().write_text(saved);self.record().chmod(0o644)
        self.assertEqual(polling.discover(self.store),[])

    def test_live_current_process_can_be_registered(self):
        with patch.object(polling,'PROC',Path('/proc')):
            polling.register(self.store,'codex',self.sid,os.getpid())
            self.assertEqual(polling.discover(self.store)[0]['pid'],os.getpid())


if __name__=='__main__':unittest.main()
