"""Test k6 acceptance gating against synthetic responses; not a load benchmark.

Requires Python 3 and k6 on PATH. No database, credentials or dependencies needed.
"""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import parse_qs, urlsplit
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--script', type=Path, default=Path(__file__).with_name('k6.js'))
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)

    class Stub(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'

        def log_message(self, *_args):
            pass

        def respond(self, code, body):
            payload = json.dumps(body).encode() if code not in (204, 304) else b''
            self.send_response(code)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(payload)))
            self.send_header('Connection', 'close')
            self.close_connection = True
            self.end_headers()
            self.wfile.write(payload)

        def do_GET(self):
            # Supply bounded, distinct, versioned targets to the production setup.
            after = int(parse_qs(urlsplit(self.path).query).get('after', ['0'])[0])
            self.respond(304 if self.server.case == 'redirect' and '?' not in self.path else 200,
                         [{'id': i, 'version': 1} for i in range(after + 1, min(after + 101, 401))])

        def do_POST(self):
            self.rfile.read(int(self.headers.get('Content-Length', '0')))
            self.respond(200 if self.server.case in ('wrong-post', 'delete-setup') else 201, {'id': 2})

        def do_PUT(self):
            body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
            if self.server.case == 'stale-put' or body.get('version') != self.server.version:
                self.respond(409, {'error': 'version conflict'})
                return
            self.server.version += 1
            self.respond(200, {'id': 1, 'version': self.server.version})

        def do_DELETE(self):
            self.server.deletes += 1
            self.respond(204, None)

    results = []
    with tempfile.TemporaryDirectory(prefix='gogolook-gate-') as temp:
        temp = Path(temp)
        shutil.copyfile(args.script, temp / 'k6.js')
        (temp / 'tokens.json').write_text(json.dumps([{'tenant': 101, 'token': 'synthetic-gate-fixture'}]))
        (temp / 'regression.js').write_text('''import { mixed, setup as setupProduction, options as production } from './k6.js';
import { check } from 'k6';
export function setup() { return __ENV.GATE_CONTRACT === '1' ? {} : setupProduction(); }
export const options = {
  scenarios: { gate: { executor: 'shared-iterations', vus: 1, iterations: 3, maxDuration: '10s' } },
  // Timing and arrival rate are not the subject of this correctness regression.
  thresholds: Object.fromEntries(Object.entries(production.thresholds).filter(([k]) => k === 'checks' || k === 'http_req_failed')),
};
export default function(ids) {
  if (__ENV.GATE_CONTRACT === '1') {
    const expected = { checks: ['rate==1'], http_req_failed: ['rate==0'], dropped_iterations: ['count==0'] };
    check(production, {
      'only correctness and delivery thresholds remain': p =>
        Object.keys(p.thresholds).length === 3 && Object.entries(expected).every(([key, value]) =>
          JSON.stringify(p.thresholds[key]) === JSON.stringify(value)),
      'latency percentiles remain reported': p =>
        p.summaryTrendStats.includes('p(95)') && p.summaryTrendStats.includes('p(99)'),
    });
    return;
  }
  Math.random = () => Number(__ENV.DRAW); mixed(ids);
}
''', encoding='utf-8')
        for case, draw, should_pass in [('threshold-contract', '0', True), ('get', '0', True), ('post', '.81', True), ('put', '.9', True), ('delete', '.9999', True), ('redirect', '0', False), ('wrong-post', '.81', False), ('delete-setup', '.9999', False), ('stale-put', '.9', False)]:
            with ThreadingHTTPServer(('127.0.0.1', 0), Stub) as server:
                server.case, server.deletes = case, 0
                server.version = 1
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                summary = (args.output / (case + '.json')).resolve()
                command = ['k6', 'run', '--quiet', '--summary-export', str(summary), '-e', 'MODE=slo', '-e', 'LIMIT=', '-e', 'SKEW=0', '-e', 'TOKENS=./tokens.json', '-e', 'BASE=http://127.0.0.1:' + str(server.server_port), '-e', 'DRAW=' + draw, '-e', 'GATE_CONTRACT=' + ('1' if case == 'threshold-contract' else '0'), 'regression.js']
                try:
                    process = subprocess.run(command, cwd=temp, capture_output=True, text=True, encoding='utf-8', errors='replace', timeout=30)
                finally:
                    server.shutdown()
                    thread.join()
                (args.output / (case + '.log')).write_text(process.stdout + process.stderr, encoding='utf-8')
                metrics = json.loads(summary.read_text())['metrics']
                checks = metrics.get('checks', {})
                detected = process.returncode == 0 if should_pass else process.returncode == 99 and checks.get('fails', 0) > 0 and checks.get('thresholds', {}).get('rate==1') is True
                # Legacy summary export uses true for a failed threshold.
                if should_pass:
                    detected = detected and checks.get('fails', 0) == 0 and checks.get('passes', 0) > 0
                if case == 'redirect':
                    detected = detected and metrics['http_req_failed']['value'] > 0
                if case == 'delete':
                    detected = detected and server.deletes == 3
                if case == 'delete-setup':
                    detected = detected and server.deletes == 0
                results.append({'case': case, 'exit_code': process.returncode, 'regression_pass': detected, 'delete_requests': server.deletes})
                print(json.dumps(results[-1]), flush=True)
    (args.output / 'results.json').write_text(json.dumps(results, indent=2) + '\n', encoding='utf-8')
    if not all(result['regression_pass'] for result in results):
        raise SystemExit(1)


if __name__ == '__main__':
    main()
