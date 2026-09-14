"""Real converter adapter. Rendezvous delays the source bytes, not conversion."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

c = json.loads(Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text())
i = c['input']
source = Path(i['source']).read_bytes()
assert hashlib.sha256(source).hexdigest() == i['source_sha256']
assert i['format'] == 'webp' and i['lossless'] and i['exact']
p = subprocess.Popen(['/usr/bin/magick', 'png:-', '-define', 'webp:lossless=true',
                      '-define', 'webp:exact=true', i['output']], stdin=subprocess.PIPE)
record = Path(i['records']) / (c['step_id'] + '.json')
temporary = record.with_suffix('.tmp')
temporary.write_text(json.dumps({'pid': p.pid, 'context': c}))
temporary.rename(record)
deadline = time.monotonic() + 15
while not Path(i['release']).exists():
    if time.monotonic() > deadline:
        p.kill()
        p.wait()
        sys.exit(70)
    time.sleep(.01)
p.communicate(source)
sys.exit(p.returncode)
