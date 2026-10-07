"""Test the standalone release script with fake cloud and CLI commands."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class DeployTest(unittest.TestCase):
    def test_standalone_build_source_images_and_failures(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            app = root / 'agentray'
            (app / 'infra/gce').mkdir(parents=True)
            shutil.copy2(ROOT / 'infra/gce/deploy.sh', app / 'infra/gce/deploy.sh')
            binary = root / 'bin'
            binary.mkdir()
            log = root / 'calls'
            for name in ('gcloud', '2server', 'python3'):
                body = '#!/bin/bash\necho "' + name + ' $*" >> "$CALLS"\n'
                if name == 'gcloud':
                    body += 'if [ "$1 $2" = "builds submit" ] && [ "$FAIL" = build ]; then exit 1; fi\n'
                elif name == 'python3':
                    body += 'if [ "$FAIL" = prepare ]; then exit 1; fi\nmkdir -p "$2"\n'
                else:
                    body += '''if [ "$1" = get ] && [ "$FAIL" = preflight ]; then exit 1; fi
if [ "$1" = deploy ] && [ "$3" = 2server/api.yaml ] && [ "$FAIL" = api ]; then exit 1; fi
'''
                (binary / name).write_text(body + 'exit 0\n')
                (binary / name).chmod(0o755)

            def run(args, fail=''):
                log.write_text('')
                env = dict(os.environ, PATH=str(binary) + ':' + os.environ['PATH'],
                           CALLS=str(log), FAIL=fail)
                result = subprocess.run(['/bin/bash', str(app / 'infra/gce/deploy.sh'), *args],
                                        cwd=root, env=env, capture_output=True, text=True)
                return result, log.read_text()

            result, calls = run(['--env', 'prod', '--skip-build'])
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('2server deploy -f 2server/api.yaml --apply', calls)
            self.assertIn('2server deploy -f 2server/web.yaml --apply', calls)
            self.assertNotIn('--image', calls)
            self.assertNotIn('gcloud', calls)
            self.assertNotIn('python3', calls)

            result, calls = run(['--env', 'prod', '--tag', 'release-test'])
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertLess(calls.index('2server get'), calls.index('gcloud builds submit'))
            self.assertLess(calls.index('python3'), calls.index('gcloud builds submit'))
            self.assertLess(calls.index('gcloud builds submit'), calls.index('2server deploy'))
            self.assertIn('agentray-api:release-test', calls)
            self.assertIn('agentray-web:release-test', calls)
            self.assertNotIn('compute ssh', calls)

            for fail in ('preflight', 'prepare', 'build', 'api'):
                result, calls = run(['--env', 'prod', '--tag', 'release-test'], fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('2server deploy -f 2server/web.yaml', calls)
                if fail != 'api':
                    self.assertNotIn('2server deploy', calls)
            for args in (['--env', 'dev'], ['--env'], ['--env', 'prod', '--tag']):
                result, calls = run(args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, '')


if __name__ == '__main__':
    unittest.main()
