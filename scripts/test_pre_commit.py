"""Exercise the actual Git hook in disposable local repositories."""
from pathlib import Path
import os, shlex, shutil, subprocess, sys, tempfile, unittest

SOURCE = Path(__file__).resolve().parent.parent

class PreCommitTests(unittest.TestCase):
    def setUp(self):
        # Fixtures are retained for inspection; the harness does not delete trees.
        parent = Path(os.environ.get('HOOK_TEST_WORK', tempfile.gettempdir()))
        parent.mkdir(parents=True, exist_ok=True)
        self.repo = Path(tempfile.mkdtemp(prefix='gogolook hook ', dir=parent))
        self.git('init', '-q')
        self.git('config', 'user.name', 'Local Hook Test')
        self.git('config', 'user.email', 'hook-test@example.invalid')
        self.git('config', 'core.hooksPath', '.githooks')
        shutil.copytree(SOURCE/'.githooks',self.repo/'.githooks')
        shutil.copytree(SOURCE/'scripts',self.repo/'scripts')
        shutil.copyfile(SOURCE/'.gitattributes',self.repo/'.gitattributes')
        self.git('config', 'core.autocrlf', 'true')
        self.git('add', '.gitattributes')
        (self.repo/'.githooks/pre-commit').chmod(0o755)
        (self.repo/'go.mod').write_text('module example.invalid/hookfixture\n\ngo 1.26.0\n')

    def git(self,*args):
        return subprocess.run(['git',*args],cwd=self.repo,capture_output=True,text=True,check=True)

    def stage(self,source):
        (self.repo/'value.go').write_text(source)
        self.git('add','go.mod','value.go')

    def commit(self):
        return subprocess.run(['git','commit','-qm','local hook fixture'],cwd=self.repo,capture_output=True,text=True)

    def test_staged_format_error_is_blocked_even_if_worktree_is_fixed(self):
        self.stage('package value\nfunc Value()int{return 1}\n')
        (self.repo/'value.go').write_text('package value\n\nfunc Value() int { return 1 }\n')
        result=self.commit()
        self.assertNotEqual(result.returncode,0,'commit accepted unformatted staged Go')
        self.assertIn('gofmt',result.stdout+result.stderr)

    def test_clean_staged_source_passes_despite_unstaged_syntax_error(self):
        self.stage('package value\n\nfunc Value() int { return 1 }\n')
        (self.repo/'value.go').write_text('this is broken and is not staged\n')
        result=self.commit()
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)

    def test_failing_staged_test_blocks_commit(self):
        self.stage('package value\n\nfunc Value() int { return 1 }\n')
        (self.repo/'value_test.go').write_text('package value\n\nimport "testing"\n\nfunc TestValue(t *testing.T) { t.Fatal("intentional assertion failure") }\n')
        self.git('add','value_test.go')
        result=self.commit()
        self.assertNotEqual(result.returncode,0,'commit accepted failing tests')
        self.assertIn('intentional assertion failure',result.stdout+result.stderr)

    def test_unusable_python3_falls_back_to_working_python(self):
        self.stage('package value\n\nfunc Value() int { return 1 }\n')
        fake_bin = self.repo / 'fake bin'
        fake_bin.mkdir()
        stub = fake_bin / 'python3'
        stub.write_bytes(b'#!/bin/sh\nexit 126\n')
        stub.chmod(0o755)
        # Own both interpreter candidates so the test does not depend on a
        # separate `python` alias being installed on the machine running it.
        fallback = fake_bin / 'python'
        executable = shlex.quote(Path(sys.executable).as_posix())
        fallback.write_bytes(f'#!/bin/sh\nexec {executable} "$@"\n'.encode())
        fallback.chmod(0o755)
        env = os.environ.copy()
        env['PATH'] = str(fake_bin) + os.pathsep + env['PATH']
        result = subprocess.run(['git','commit','-qm','local hook fixture'],cwd=self.repo,env=env,capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)

    def test_database_credentials_are_not_forwarded_to_unit_checks(self):
        self.stage('package value\n\nfunc Value() int { return 1 }\n')
        (self.repo/'value_test.go').write_text('package value\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestEnvironment(t *testing.T) {\n\tif os.Getenv("TEST_DATABASE_URL") != "" || os.Getenv("TEST_ALLOW_DATABASE") != "" {\n\t\tt.Fatal("database environment leaked")\n\t}\n}\n')
        self.git('add','value_test.go')
        old={k:os.environ.get(k) for k in ['TEST_DATABASE_URL','TEST_ALLOW_DATABASE']}
        try:
            os.environ['TEST_DATABASE_URL']='postgres://must-not-be-used'
            os.environ['TEST_ALLOW_DATABASE']='tasks_test'
            result=self.commit()
        finally:
            for k,v in old.items():
                if v is None:os.environ.pop(k,None)
                else:os.environ[k]=v
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)

if __name__=='__main__':unittest.main()
