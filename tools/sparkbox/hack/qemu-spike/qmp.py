import json, socket, sys, time
class QMP:
    def __init__(self, path):
        self.s = socket.socket(socket.AF_UNIX); self.s.connect(path)
        self.f = self.s.makefile('rw', encoding='utf-8', newline='\n')
        self.f.readline()  # greeting
        self.cmd('qmp_capabilities')
    def cmd(self, ex, **args):
        self.f.write(json.dumps({'execute': ex, 'arguments': args} if args else {'execute': ex}) + '\n')
        self.f.flush()
        while True:
            r = json.loads(self.f.readline())
            if 'event' in r: continue
            if 'error' in r: raise RuntimeError(f"{ex}: {r['error']}")
            return r.get('return')
if __name__ == '__main__':
    q = QMP(sys.argv[1]); op = sys.argv[2]
    if op == 'snapshot':
        q.cmd('stop')
        q.cmd('migrate', uri='file:' + sys.argv[3])
        for _ in range(300):
            st = q.cmd('query-migrate')['status']
            if st in ('completed', 'failed', 'cancelled'): print('MIGRATE_STATUS=' + st); break
            time.sleep(0.1)
        q.cmd('quit')
    elif op == 'balloon':
        q.cmd('qom-set', path='/machine/peripheral/balloon0', property='guest-stats-polling-interval', value=1)
        time.sleep(3)
        st = q.cmd('qom-get', path='/machine/peripheral/balloon0', property='guest-stats')
        print('BALLOON_STATS=' + json.dumps(st))
        print('BALLOON_ACTUAL=' + json.dumps(q.cmd('query-balloon')))
        q.cmd('balloon', value=768*1024*1024)
        time.sleep(3)
        print('BALLOON_AFTER_INFLATE=' + json.dumps(q.cmd('query-balloon')))
    elif op == 'cont':
        q.cmd('cont'); print('CONT_OK')
