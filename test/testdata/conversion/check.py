"""Finish checker independently decodes the actual source and output with Pillow."""
import hashlib
import json
import os
from pathlib import Path
import sys
from PIL import Image

i = json.loads(Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text())['input']
try:
    assert hashlib.sha256(Path(i['source']).read_bytes()).hexdigest() == i['source_sha256']
    with Image.open(i['source']) as source, Image.open(i['output']) as result:
        assert result.format == 'WEBP'
        assert source.size == result.size
        assert source.convert('RGBA').tobytes() == result.convert('RGBA').tobytes()
except (OSError, AssertionError):
    sys.exit(1)
