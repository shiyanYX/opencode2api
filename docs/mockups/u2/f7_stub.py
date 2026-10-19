import json, os, sys, http.server, socketserver, threading, time

PORT = int(sys.argv[1])
HTML = sys.argv[2]
SSE = (len(sys.argv) > 3 and sys.argv[3] == 'sse')
EMPTY = (len(sys.argv) > 4 and sys.argv[4] == 'empty')
STATE = '/tmp/c5ui7/alert.json'

def alert():
    try: return json.load(open(STATE))
    except Exception: return {"active": False}

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *a): pass
    def _j(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        p = self.path.split('?')[0]
        if p in ('/', '/index.html', '/admin.html'):
            b = open(HTML, 'rb').read()
            self.send_response(200); self.send_header('Content-Type', 'text/html; charset=utf-8')
            self.send_header('Content-Length', str(len(b))); self.end_headers(); self.wfile.write(b); return
        if p == '/api/nodes':
            nodes = [] if EMPTY else [
                {"fingerprint": "fp1", "name": "n-A", "state": "available", "latency_ms": 120, "active": True, "manual": False},
                {"fingerprint": "fp2", "name": "n-B", "state": "dead", "latency_ms": -1, "active": False, "manual": False}]
            self._j({"nodes": nodes, "node_alert": alert()}); return
        if p == '/api/call-log':
            self._j([{"req_id": "req-verify-1", "ts": "2026-10-19T07:00:00Z", "status": "ok",
                      "model": "glm-5.2", "duration_ms": 1234, "path": "/v1/chat/completions",
                      "node_names": ["n-A"], "nodes": ["n-A"], "attempts": 1,
                      "events": [{"ts": "2026-10-19T07:00:00Z", "type": "complete", "node": "n-A", "detail": "ok"}]}]); return
        if p == '/api/logs/stream':
            if SSE:
                self.send_response(200); self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Cache-Control', 'no-cache'); self.end_headers()
                try:
                    for _ in range(60):
                        self.wfile.write(b': keepalive\n\n'); self.wfile.flush(); time.sleep(1)
                except Exception:
                    pass
                return
            # 与 stub.py 完全一致：200 + application/json（EventSource 会报 MIME 错）
            self._j({}); return
        self._j({})

socketserver.ThreadingTCPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer(('127.0.0.1', PORT), H).serve_forever()
