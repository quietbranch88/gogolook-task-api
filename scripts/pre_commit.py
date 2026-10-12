"""Check the staged snapshot without changing the index or working tree."""
from pathlib import Path, PurePosixPath
import io
import os
import subprocess
import sys
import tarfile
import tempfile


def capture(args, cwd):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True).stdout


def main():
    root = Path(capture(['git', 'rev-parse', '--show-toplevel'], None).decode().strip())
    changed = capture(['git', 'diff', '--cached', '--name-only', '-z'], root).decode().split('\0')
    if not any(p.endswith(('.go', '.sql')) or p in ('go.mod', 'go.sum') for p in changed):
        print('pre-commit: no staged Go/module/SQL changes; Go checks skipped.')
        return 0
    tree = capture(['git', 'write-tree'], root).decode().strip()
    archive = capture(['git', 'archive', '--format=tar', tree], root)
    # Extract only regular files into a newly owned directory. Reject links and
    # paths escaping it; never run checks against unstaged working-tree content.
    with tempfile.TemporaryDirectory(prefix='gogolook-staged-') as temp:
        snapshot = Path(temp).resolve()
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            for entry in tar.getmembers():
                parts = PurePosixPath(entry.name).parts
                if entry.name.startswith('/') or '..' in parts or any(':' in p or '\\' in p for p in parts):
                    raise ValueError('unsafe path in staged archive')
                path = snapshot.joinpath(*parts)
                if not path.resolve().is_relative_to(snapshot):
                    raise ValueError('staged path escapes snapshot')
                if entry.isdir():
                    path.mkdir(parents=True, exist_ok=True)
                elif entry.isfile():
                    path.parent.mkdir(parents=True, exist_ok=True)
                    with tar.extractfile(entry) as source:
                        path.write_bytes(source.read())
                else:
                    raise ValueError('staged symlinks/special files are unsupported by this hook')
        go_files = sorted(str(p) for p in snapshot.rglob('*.go') if 'vendor' not in p.relative_to(snapshot).parts)
        if go_files:
            unformatted = capture(['gofmt', '-l', *go_files], snapshot).decode().strip()
            if unformatted:
                print('pre-commit: gofmt required on staged source. Format and stage these files:', file=sys.stderr)
                for path in unformatted.splitlines():
                    print(Path(path).relative_to(snapshot), file=sys.stderr)
                return 1
        env = os.environ.copy()
        env.pop('TEST_DATABASE_URL', None)
        env.pop('TEST_ALLOW_DATABASE', None)
        env['GOWORK'] = 'off'
        env['GOFLAGS'] = '-mod=readonly'
        print('pre-commit: staged-source vet and unit/HTTP tests; real database tests are excluded.', flush=True)
        for command in (['go', 'vet', './...'], ['go', 'vet', '-tags', 'loadtest', './...'], ['go', 'test', '-count=1', './...']):
            result = subprocess.run(command, cwd=snapshot, env=env)
            if result.returncode:
                return result.returncode
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print('pre-commit failed: '+str(exc), file=sys.stderr)
        raise SystemExit(1)
