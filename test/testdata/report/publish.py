"""Publish real S3 bytes with destination verification and a stable effect key."""
import hashlib,json,os,pathlib,subprocess,sys,time
c=json.loads(pathlib.Path(os.environ['TODOABLE_CONTEXT_PATH']).read_text());d=c['input']['data'];w=c['input']['occurrence']
auth=json.loads(pathlib.Path(d['auth_file']).read_text())
effect=hashlib.sha256((c['task_id']+'\n'+d['source_sha256']+'\n'+w['window_start']+'\n'+w['window_end']).encode()).hexdigest()
url=d['endpoint']+'/todoable-reports/'+effect+'.json'
base=['/usr/bin/curl','--silent','--show-error','--max-time','10','--aws-sigv4','aws:amz:us-east-1:s3','--user',auth['access']+':'+auth['secret']]
report=pathlib.Path(c['run_dir'])/'report.json'; content=report.read_bytes()
def request(args):
 p=subprocess.run(base+args,capture_output=True)
 if p.returncode: raise RuntimeError('S3 request failed with exit '+str(p.returncode))
 return p
head=request(['-I','-w','\n%{http_code}',url])
status=head.stdout.rsplit(b'\n',1)[-1]
if status==b'200':
 got=request(['--fail',url]).stdout
 assert got==content and ('x-amz-meta-effect-key: '+effect).encode() in head.stdout.lower()
elif status==b'404':
 request(['--fail-with-body','-T',str(report),'-H','x-amz-meta-effect-key: '+effect,url])
else:
 raise RuntimeError('destination query failed: HTTP '+status.decode())
# Only reached after actual successful remote PUT or independently verified GET.
record=pathlib.Path(d['records'])/(c['step_id']+'.json'); record.write_text(json.dumps({'effect_key':effect,'run_id':c['run_id'],'step_id':c['step_id']}))
if w['window_end']==d.get('hold_end'):
 if d.get('escape'):
  child=subprocess.Popen([sys.executable,'-c',"import pathlib,time,sys; p=pathlib.Path(sys.argv[1]);\nwhile not p.exists(): time.sleep(.01)",d['release']],start_new_session=True)
  pathlib.Path(d['child_pid']).write_text(str(child.pid));sys.exit(0)
 while not pathlib.Path(d['release']).exists():time.sleep(.01)
