"""Test oracle: frozen source-derived dimensions and RGBA hashes, no engine results."""
import hashlib
import json
from pathlib import Path
import sys
from PIL import Image

manifest = json.loads(Path(sys.argv[1]).read_text())
for item in manifest['sources']:
    source = Path(sys.argv[2]) / item['source']
    assert hashlib.sha256(source.read_bytes()).hexdigest() == item['sha256']
    with Image.open(source) as image:
        assert image.size == (item['width'], item['height'])
        assert hashlib.sha256(image.convert('RGBA').tobytes()).hexdigest() == item['rgba_sha256']
    if len(sys.argv) > 3:
        with Image.open(Path(sys.argv[3]) / (item['source'] + '.webp')) as image:
            assert image.format == 'WEBP'
            assert image.size == (item['width'], item['height'])
            assert hashlib.sha256(image.convert('RGBA').tobytes()).hexdigest() == item['rgba_sha256']
