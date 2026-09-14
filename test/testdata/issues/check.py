"""Acceptance requirements precede model execution and live outside its checkout."""
import importlib.util
import json
import os
from pathlib import Path
import sys

i = json.loads(Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text())['input']
spec = importlib.util.spec_from_file_location('human_size', Path(i['repo']) / 'human_size.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    if i['event']['body']['action'] == 'reopened':
        assert module.human_size(1536, precision=2) == '1.50 KiB'
        assert module.human_size(1024, precision=3) == '1.000 KiB'
    else:
        assert module.human_size(0) == '0 B'
        assert module.human_size(23) == '23 B'
        assert module.human_size(1024) == '1.0 KiB'
        assert module.human_size(1536) == '1.5 KiB'
except (AssertionError, TypeError):
    sys.exit(1)
