import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location("compat",Path(__file__).parents[1]/"sglang_compat.py")
compat=importlib.util.module_from_spec(spec);spec.loader.exec_module(compat)


class CompatibilityTest(unittest.TestCase):
    def test_wheel_crlf_is_preserved(self):
        original=(b"def method(self):\n    if True:\n"+compat.BEFORE).replace(b"\n",b"\r\n")
        with tempfile.TemporaryDirectory() as folder:
            target=Path(folder)/"batch.py";target.write_bytes(original)
            with patch.object(compat,"SOURCE_SHA256",hashlib.sha256(original).hexdigest()):
                self.assertTrue(compat.repair(target));self.assertFalse(compat.repair(target))
                self.assertIn(compat.AFTER.replace(b"\n",b"\r\n"),target.read_bytes())

    def test_checksum_gate_and_idempotence(self):
        original=b"def method(self):\n    if True:\n"+compat.BEFORE
        with tempfile.TemporaryDirectory() as folder:
            target=Path(folder)/"batch.py";target.write_bytes(original)
            with self.assertRaises(RuntimeError):compat.repair(target)
            self.assertEqual(target.read_bytes(),original)
            with patch.object(compat,"SOURCE_SHA256",hashlib.sha256(original).hexdigest()):
                self.assertTrue(compat.repair(target))
                self.assertFalse(compat.repair(target))
                modified=target.read_bytes()
                self.assertIn(compat.AFTER,modified)
                target.write_bytes(modified+b"# unexpected local edit\n")
                with self.assertRaises(RuntimeError):compat.repair(target)
