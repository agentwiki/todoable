"""Real SQLite aggregation of a fixed repository history; no engine-state oracle."""
import datetime,hashlib,json,os,pathlib,sqlite3,sys
c=json.loads(pathlib.Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text()); d=c['input']['data']; w=c['input']['occurrence']
raw=pathlib.Path(d['source']).read_bytes()
assert hashlib.sha256(raw).hexdigest()==d['source_sha256']
def stamp(s): return int(datetime.datetime.fromisoformat(s.replace('Z','+00:00')).timestamp())
out=pathlib.Path(c['run_dir'])/'report.json'
if c['stage']=='agent':
 db=sqlite3.connect(pathlib.Path(c['run_dir'])/'aggregation.sqlite')
 db.execute('CREATE TABLE IF NOT EXISTS commits(id TEXT PRIMARY KEY, at INTEGER NOT NULL)')
 db.executemany('INSERT OR IGNORE INTO commits VALUES(?,?)',[(h,int(t)) for h,t in (line.split() for line in raw.decode().splitlines())]); db.commit()
 rows=[r[0] for r in db.execute('SELECT id FROM commits WHERE at>=? AND at<? ORDER BY id',(stamp(w['window_start']),stamp(w['window_end'])))]
 report={'window_start':w['window_start'],'window_end':w['window_end'],'source_sha256':d['source_sha256'],'commits':rows,'count':len(rows)}
 out.write_text(json.dumps(report,sort_keys=True)+'\n'); db.close()
else:
 try:
  report=json.loads(out.read_text()); rows=sorted(h for h,t in (line.split() for line in raw.decode().splitlines()) if stamp(w['window_start'])<=int(t)<stamp(w['window_end']))
  assert report=={'window_start':w['window_start'],'window_end':w['window_end'],'source_sha256':d['source_sha256'],'commits':rows,'count':len(rows)}
 except (OSError,AssertionError): sys.exit(1)
