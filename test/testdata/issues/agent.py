"""Actual Codex adapter; provider-specific behavior lives outside the engine."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

c = json.loads(Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text())
i = c['input']
instruction = ('Work only in this temporary checkout. Do not access external services, '
               'send messages, alter tests, or commit. Fix human_size.py to satisfy this '
               'actual issue requirement:\n' + i['event']['body']['issue']['body'])
if i['mode'] == 'unmet':
    instruction = ('Read human_size.py and explain the defect briefly. Do not edit any file '
                   'and do not run commands that write files. This invocation is review-only.')
record = Path(i['records']) / (c['step_id'] + '.json')
record.write_text(json.dumps({'context': c, 'state': 'started'}))
p = subprocess.run([i['codex'], 'exec', '--ignore-user-config', '--ignore-rules',
                    '--ephemeral', '--sandbox', 'workspace-write', '--json',
                    '-C', i['repo'], instruction], timeout=120)
record.write_text(json.dumps({'context': c, 'state': 'model_finished', 'exit_code': p.returncode}))
if p.returncode != 0:
    sys.exit(p.returncode)
if i['mode'] == 'unknown':
    # The genuine model has returned, but the adapter has not returned its Step
    # result yet. Killing the daemon here leaves a real uncertain execution.
    Path(i['rendezvous']).write_text(c['step_id'])
    while True:
        time.sleep(.1)
sys.exit(19 if i['mode'] == 'nonzero' else 0)
