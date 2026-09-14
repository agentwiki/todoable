"""Isolated real Forgejo issue/webhook/git + actual Codex + public todoable CLI E2E."""
import base64
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import urllib.request
import uuid

IMAGE = 'codeberg.org/forgejo/forgejo@sha256:a3e33d03e771d3e58b27de5573c3a25dc4f670583a6724c1878a6d0bbecf3556'
BIN, ROOT, MODE = sys.argv[1], Path(sys.argv[2]), sys.argv[3]
ASSETS = Path(__file__).parent
DATA = ROOT / 'data'
NAME = 'todoable-issues-' + uuid.uuid4().hex
USER = 'todoable-test'
PASSWORD = secrets.token_urlsafe(24)
AUTH = 'Basic ' + base64.b64encode((USER + ':' + PASSWORD).encode()).decode()
daemon = None
daemon_log = None
created = False


def command(args, **kwargs):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=150, **kwargs)
    assert result.returncode == 0, (args[:2], result.returncode, result.stdout.decode(errors='replace'), result.stderr.decode(errors='replace'))
    return result.stdout


def poll(check, seconds=150):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.05)
    raise AssertionError('timed out waiting for real workflow observation')


def cli(*args):
    raw = command([BIN, '--data-dir', str(DATA), *args])
    value = json.loads(raw)
    assert value['protocol_version'] == 1
    return value


def api(path, body=None, method='POST'):
    request = urllib.request.Request(BASE + '/api/v1/' + path,
                                     data=None if body is None else json.dumps(body).encode(),
                                     headers={'Content-Type': 'application/json', 'Authorization': AUTH},
                                     method=method)
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def events():
    result = subprocess.run(['docker', 'exec', NAME, 'cat', '/tmp/events.jsonl'], capture_output=True)
    if result.returncode:
        return []
    return [json.loads(line) for line in result.stdout.splitlines() if line.strip()]


def event_for(number, action):
    def find():
        return next((item for item in events() if item['body']['number'] == number and item['body']['action'] == action), None)
    event = poll(find, 20)
    assert event['event'] == 'issues' and event['delivery']
    assert event['body']['issue']['number'] == number
    return event


def start_daemon():
    global daemon, daemon_log
    daemon_log = open(ROOT / ('daemon-' + uuid.uuid4().hex + '.log'), 'wb')
    daemon = subprocess.Popen([BIN, '--data-dir', str(DATA), 'daemon'], stdout=daemon_log, stderr=daemon_log)
    return daemon


def stop_daemon(kill=False):
    global daemon
    if daemon is not None:
        if kill:
            daemon.kill()
        else:
            daemon.send_signal(signal.SIGTERM)
        result = daemon.wait(timeout=20)
        assert result == (-9 if kill else 0), ('daemon exit', result)
        daemon_log.close()
        daemon = None


def view(receipt):
    return cli('run', 'show', receipt['run_id'], '--json')


def finished(receipt):
    return poll(lambda: (v if (v := view(receipt))['state'] in ('succeeded', 'failed', 'blocked') else None))


def snapshot():
    with sqlite3.connect(DATA / 'todoable.db') as db:
        return {table: db.execute('SELECT * FROM ' + table + ' ORDER BY rowid').fetchall()
                for table in ('tasks', 'task_versions', 'submissions', 'runs', 'steps', 'repeat_budgets')}


def submit(task_id, event, repo, mode):
    request = {'task_id': task_id, 'input_key': 'issue-' + str(event['body']['number']),
               'concurrency_key': str(repo), 'task_version': 1,
               'input': {'event': event, 'repo': str(repo), 'mode': mode, 'codex': CODEX,
                         'records': str(ROOT / 'records'), 'rendezvous': str(ROOT / ('gate-' + task_id))}}
    path = ROOT / (task_id + '-input.json')
    path.write_text(json.dumps(request))
    return cli('run', 'submit', str(path)), path


def task(task_id, repo):
    definition = {'version': 1, 'id': task_id, 'workdir': str(repo),
                  'prompt': 'Implement the received issue in this isolated checkout',
                  'agent': ['/usr/bin/python3', str(ASSETS / 'agent.py')],
                  'finish': {'check': ['/usr/bin/python3', str(ASSETS / 'check.py')], 'max_calls': 1},
                  'limits': {'agent_timeout': '3m', 'run_timeout': '5m'}}
    path = ROOT / (task_id + '-task.json')
    path.write_text(json.dumps(definition))
    assert cli('task', 'register', str(path))['task_version'] == 1


def assert_steps(value, expected, agent_exit):
    assert value['stage'] == expected, value
    assert value['calls_used'] == 1 and value['repeat_remaining'] == 0, value
    assert [s['stage'] for s in value['steps']] == ['finish_check', 'agent', 'finish_check'], value
    assert value['steps'][0]['result']['exit_code'] == 1
    assert value['steps'][1]['result']['kind'] == 'exited'
    assert value['steps'][1]['result']['exit_code'] == agent_exit
    assert value['steps'][2]['result']['exit_code'] == (0 if expected == 'succeeded' else 1)
    step = value['steps'][1]['step_id']
    logs = (DATA / 'runs' / value['run_id'] / 'steps' / step / 'stdout').read_text()
    model_events = [json.loads(line) for line in logs.splitlines() if line.startswith('{')]
    assert any(item['type'] == 'turn.completed' and item['usage']['output_tokens'] > 0 for item in model_events), logs


def replay(receipt, path):
    before = snapshot()
    calls = sorted((ROOT / 'records').glob('*.json'))
    repeated = cli('run', 'submit', str(path))
    assert repeated['deduplicated'] and repeated['submission_id'] == receipt['submission_id'] and repeated['run_id'] == receipt['run_id']
    time.sleep(1.1)
    assert snapshot() == before
    assert sorted((ROOT / 'records').glob('*.json')) == calls


try:
    CODEX = shutil.which('codex')
    assert CODEX and command([CODEX, '--version']).strip() == b'codex-cli 0.154.0'
    (ROOT / 'receiver.go').write_bytes((ASSETS / 'receiver.go').read_bytes())
    command(['go', 'build', '-o', str(ROOT / 'receiver'), str(ROOT / 'receiver.go')], env={**os.environ, 'CGO_ENABLED': '0'})
    command(['docker', 'run', '-d', '--rm', '--name', NAME, '-p', '127.0.0.1::3000',
             '-e', 'FORGEJO__security__INSTALL_LOCK=true', '-e', 'FORGEJO__server__DISABLE_SSH=true',
             '-e', 'FORGEJO__service__DISABLE_REGISTRATION=true', '-e', 'FORGEJO__mailer__ENABLED=false',
             '-e', 'FORGEJO__actions__ENABLED=false', '-e', 'FORGEJO__webhook__ALLOWED_HOST_LIST=loopback', IMAGE])
    created = True
    BASE = 'http://' + command(['docker', 'port', NAME, '3000/tcp']).decode().strip()

    def ready():
        try:
            with urllib.request.urlopen(BASE + '/api/v1/version', timeout=1) as response:
                return json.load(response)['version'] == '16.0.4+gitea-1.22.0'
        except OSError:
            return False

    poll(ready, 30)
    command(['docker', 'exec', '-u', 'git', NAME, 'forgejo', 'admin', 'user', 'create',
             '--username', USER, '--password', PASSWORD, '--email', 'todoable@example.invalid',
             '--admin', '--must-change-password=false'])
    api('user/repos', {'name': 'test-project', 'private': False, 'auto_init': False})
    prefix = 'repos/' + USER + '/test-project/'
    command(['docker', 'cp', str(ROOT / 'receiver'), NAME + ':/tmp/receiver'])
    command(['docker', 'exec', '-d', NAME, '/tmp/receiver'])
    api(prefix + 'hooks', {'type': 'gitea', 'active': True, 'events': ['issues'],
                           'config': {'url': 'http://127.0.0.1:9876/event', 'content_type': 'json'}})
    repo = ROOT / 'checkout'
    command(['git', 'init', '-b', 'main', str(repo)])
    command(['git', '-C', str(repo), 'config', 'user.email', 'todoable@example.invalid'])
    command(['git', '-C', str(repo), 'config', 'user.name', 'Todoable isolated test'])
    seed = 'def human_size(n):\n    return str(n) + " B"\n'
    (repo / 'human_size.py').write_text(seed)
    command(['git', '-C', str(repo), 'add', 'human_size.py'])
    command(['git', '-C', str(repo), 'commit', '-m', 'Reproduce issue in isolated test repository'])
    command(['git', '-C', str(repo), 'remote', 'add', 'origin', BASE + '/' + USER + '/test-project.git'])
    command(['git', '-C', str(repo), '-c', 'http.extraHeader=Authorization: ' + AUTH, 'push', 'origin', 'main'])
    opened_body = 'In human_size.py, human_size(n) must render 0 as "0 B", 23 as "23 B", 1024 as "1.0 KiB", and 1536 as "1.5 KiB". Use binary KiB units. Do not add optional parameters yet.'
    modes = ['normal'] if MODE == 'events' else ['nonzero', 'unmet', 'unknown']
    start_daemon()
    for index, mode in enumerate(modes):
        (repo / 'human_size.py').write_text(seed)
        number = api(prefix + 'issues', {'title': 'Format binary byte sizes ' + mode, 'body': opened_body})['number']
        event = event_for(number, 'opened')
        assert event['body']['issue']['body'] == opened_body
        task_id = 'issue-' + str(index)
        task(task_id, repo)
        receipt, path = submit(task_id, event, repo, mode)
        if mode == 'unknown':
            poll(lambda: (ROOT / ('gate-' + task_id)).exists())
            active = view(receipt)
            assert active['state'] == 'running' and active['steps'][-1]['stage'] == 'agent'
            assert active['steps'][-1]['result'] is None
            # The real patch already satisfies the independent issue oracle,
            # but the adapter has not delivered a result to the engine.
            context = ROOT / 'interrupted-oracle-context.json'
            context.write_text(json.dumps({'input': json.loads(path.read_text())['input']}))
            command(['/usr/bin/python3', str(ASSETS / 'check.py')], env={**os.environ, 'TODOABLE_CONTEXT_PATH': str(context)})
            patch = (repo / 'human_size.py').read_bytes()
            assert patch != seed.encode()
            step_id = active['steps'][-1]['step_id']
            logs = DATA / 'runs' / receipt['run_id'] / 'steps' / step_id / 'stdout'
            poll(lambda: logs.exists() and 'turn.completed' in logs.read_text())
            saved_log = logs.read_bytes()
            stop_daemon(kill=True)
            start_daemon()
            value = finished(receipt)
            assert value['state'] == 'blocked' and value['stage'] == 'blocked:outcome_unknown', value
            assert value['calls_used'] == 1 and value['repeat_remaining'] == 0
            assert len(value['steps']) == 2
            step = value['steps'][-1]
            assert step['step_id'] == step_id
            assert step['result']['kind'] == 'interrupted', step
            assert logs.read_bytes() == saved_log
            with sqlite3.connect(DATA / 'todoable.db') as db:
                assert db.execute('SELECT run_id FROM resources WHERE key=?', (str(repo),)).fetchone() == (receipt['run_id'],)
            before = snapshot()
            time.sleep(1.1)
            assert snapshot() == before and view(receipt)['state'] == 'blocked'
            replay(receipt, path)
            assert (repo / 'human_size.py').read_bytes() == patch
            assert len(list((ROOT / 'records').glob('*.json'))) == 3
        else:
            value = finished(receipt)
            assert_steps(value, 'failed:max_calls' if mode == 'unmet' else 'succeeded', 19 if mode == 'nonzero' else 0)
            if mode != 'unmet':
                assert (repo / 'human_size.py').read_text() != seed
                # Independent interpreter verifies the issue result after the engine result.
                context = ROOT / 'oracle-context.json'
                context.write_text(json.dumps({'input': json.loads(path.read_text())['input']}))
                command(['/usr/bin/python3', str(ASSETS / 'check.py')], env={**os.environ, 'TODOABLE_CONTEXT_PATH': str(context)})
            replay(receipt, path)
        assert value['input']['event'] == event and value['task_version'] == 1
        if MODE == 'events':
            api(prefix + 'issues/' + str(number), {'state': 'closed'}, 'PATCH')
            reopened_body = 'Extend human_size(n, precision=2): 1536 with precision=2 must return "1.50 KiB"; 1024 with precision=3 must return "1.000 KiB". The precision argument must control decimal places.'
            api(prefix + 'issues/' + str(number), {'body': reopened_body, 'state': 'open'}, 'PATCH')
            newer = event_for(number, 'reopened')
            assert newer['delivery'] != event['delivery'] and newer['body']['issue']['body'] == reopened_body
            reopened, reopened_path = submit(task_id, newer, repo, 'normal')
            assert reopened['submission_id'] != receipt['submission_id'] and not reopened['deduplicated']
            assert_steps(finished(reopened), 'succeeded', 0)
            replay(reopened, reopened_path)
            print('Actual opened/reopened webhook deliveries, model patches, acceptance and exact replay verified.')
    stop_daemon()
    if MODE != 'events':
        print('Actual nonzero, unmet acceptance and interrupted model adapter boundaries verified.')
finally:
    if daemon is not None:
        daemon.send_signal(signal.SIGTERM)
        try:
            daemon.wait(timeout=20)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.wait()
        daemon_log.close()
    if created:
        subprocess.run(['docker', 'rm', '-f', '-v', NAME], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
