"""Narrow, checksum-pinned compatibility fix for our SGLang 5a26fc1f wheel."""
import hashlib
import importlib.metadata
import os
from pathlib import Path
import shutil

SOURCE_SHA256 = "67bd70ee2c2a4e7f2d9ac7a59ccd5e8a8f71bd2cb8c97ad33d58ec2a5dc05218"
BEFORE = b"        self.seq_lens_sum += bs\n"
AFTER = b"""        # Talea compatibility: filter_batch/merge_batch invalidate this cache.
        # seq_lens_cpu has already been incremented above, so reconstruct the
        # sum without adding bs a second time. No device synchronization.
        if self.seq_lens_sum is None:
            self.seq_lens_sum = int(self.seq_lens_cpu.sum().item())
        else:
            self.seq_lens_sum += bs
"""


def repair(path):
    path = Path(path)
    data = path.read_bytes()
    newline = b"\r\n" if b"\r\n" in data else b"\n"
    before, after = BEFORE.replace(b"\n",newline), AFTER.replace(b"\n",newline)
    if data.count(after)==1 and hashlib.sha256(data.replace(after,before,1)).hexdigest()==SOURCE_SHA256:
        return False
    if hashlib.sha256(data).hexdigest()!=SOURCE_SHA256 or data.count(before)!=1:
        raise RuntimeError("SGLang source differs from pinned PD compatibility patch; refusing modification")
    backup = path.with_name(path.name+".talea-original-5a26fc1f")
    if not backup.exists():
        shutil.copy2(path,backup)
    target = path.with_name(path.name+".talea-next")
    target.write_bytes(data.replace(before,after,1))
    target.chmod(path.stat().st_mode)
    os.replace(target,path)
    return True


def main():
    distribution = importlib.metadata.distribution("sglang")
    if "5a26fc1f" not in distribution.version:
        print("PD batch compatibility patch not applicable to",distribution.version)
        return
    path=distribution.locate_file("sglang/srt/managers/schedule_batch.py")
    changed=repair(path)
    print("Talea PD batch sum compatibility:","applied" if changed else "already applied")


if __name__=="__main__":
    main()
