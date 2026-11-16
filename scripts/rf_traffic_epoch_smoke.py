"""Consume real native counters before and after same-PID/same-config reload on private loopback."""
from pathlib import Path
import argparse, http.client, http.server, json, os, signal, socket, subprocess, tempfile, threading, time, uuid

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--sing-box', required=True)
    args = parser.parse_args()
    binary = str(Path(args.sing_box).resolve())
    def port():
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]
    api_port, proxy_port = port(), port()
    secret = 'private-native-epoch-fixture'
    class Target(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            content = b'x' * (4096 if self.path == '/small' else 65536)
            self.send_response(200)
            self.send_header('Content-Length', str(len(content)))
            self.end_headers()
            self.wfile.write(content)
        def log_message(self, *args):
            pass
    target = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Target)
    serving = threading.Thread(target=target.serve_forever)
    serving.start()
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix='rf-traffic-epoch-') as directory:
            root = Path(directory)
            (root/'owner.json').write_text(json.dumps(dict(owner='rf-traffic-epoch-smoke', purpose=__doc__, input=binary, cleanup='scoped context')), encoding='utf-8')
            config = root/'config.json'
            config.write_text(json.dumps(dict(log=dict(disabled=True),
                inbounds=[dict(type='http', listen='127.0.0.1', listen_port=proxy_port)],
                outbounds=[dict(type='direct', tag='explicit-private-loopback')],
                route=dict(final='explicit-private-loopback'),
                experimental=dict(clash_api=dict(external_controller=f'127.0.0.1:{api_port}', secret=secret)))), encoding='utf-8')
            config.chmod(0o600)
            process = subprocess.Popen([binary, 'run', '-c', str(config)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            def point():
                connection = http.client.HTTPConnection('127.0.0.1', api_port, timeout=2)
                try:
                    connection.request('GET', '/connections', headers={'Authorization': 'Bearer '+secret})
                    response = connection.getresponse()
                    raw = response.read()
                    if response.status != 200:
                        raise RuntimeError(f'native stats HTTP{response.status}')
                    epoch = response.getheader('X-Rf-Traffic-Epoch')
                    if epoch is None or str(uuid.UUID(epoch, version=4)) != epoch:
                        raise ValueError('missing or invalid native counter epoch')
                    counters = json.loads(raw)
                    return dict(epoch=epoch, up=counters['uploadTotal'], down=counters['downloadTotal'])
                finally:
                    connection.close()
            def ready(old_epoch=None):
                deadline = time.monotonic()+10
                while True:
                    if process.poll() is not None:
                        raise RuntimeError('private native instance exited')
                    try:
                        current = point()
                        if old_epoch is None or current['epoch'] != old_epoch:
                            return current
                    except (OSError, http.client.HTTPException):
                        pass
                    if time.monotonic() >= deadline:
                        raise RuntimeError('native reload observation did not finish')
                    time.sleep(0.02)
            def flow(path, expected):
                connection = http.client.HTTPConnection('127.0.0.1', proxy_port, timeout=3)
                try:
                    connection.request('GET', f'http://127.0.0.1:{target.server_port}/{path}')
                    response = connection.getresponse()
                    content = response.read()
                    if response.status != 200 or len(content) != expected:
                        raise RuntimeError('native loopback flow was incomplete')
                finally:
                    connection.close()
            initial = ready()
            flow('small', 4096)
            first = point()
            if first['epoch'] != initial['epoch'] or first['up'] <= 0 or first['down'] < 4096:
                raise RuntimeError('native totals and epoch did not share the first manager')
            os.kill(process.pid, signal.SIGHUP)
            reloaded = ready(first['epoch'])
            flow('large', 65536)
            flow('large', 65536)
            later = point()
            if later['epoch'] != reloaded['epoch'] or later['up'] <= first['up'] or later['down'] <= first['down']:
                raise RuntimeError('reloaded native counters did not overtake the old point')
            print(json.dumps(dict(pid=process.pid, first=first, reloaded=reloaded, later=later, proxied_body_bytes=4096+2*65536, same_pid=True, same_config=True)), flush=True)
    finally:
        if process is not None:
            process.terminate()
            try:
                process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate()
        target.shutdown()
        target.server_close()
        serving.join()

if __name__ == '__main__':
    main()
