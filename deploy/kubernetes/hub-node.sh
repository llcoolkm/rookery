#!/usr/bin/env bash
# Manage token hashes in an out-of-band Kubernetes Secret; never pass a raw token.
# The hub rereads its file per request: no restart is needed. Kubelet propagation
# of Secret changes can take up to approximately two minutes.
# See docs/operations.md#adding-a-node for the complete onboarding procedure.
set -euo pipefail
usage() {
    echo 'usage: hub-node.sh [--namespace NAMESPACE] [--secret SECRET] [--backup-dir DIR] list | add NODE SHA256HEX | remove NODE' >&2
    exit 2
}
ns=rookery
secret=rookery-hub-tokens
backup_dir=${ROOKERY_HUB_BACKUP_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/rookery-hub-secret-backups}
args=()
while (($#)); do
    case "$1" in
        --namespace|-n) (($# >= 2)) || usage; ns=$2; shift 2 ;;
        --secret|-s) (($# >= 2)) || usage; secret=$2; shift 2 ;;
        --backup-dir) (($# >= 2)) || usage; backup_dir=$2; shift 2 ;;
        --help|-h) usage ;;
        --*) usage ;;
        *) args+=("$1"); shift ;;
    esac
done
((${#args[@]})) || usage
cmd=${args[0]}
case "$cmd" in
    list) ((${#args[@]} == 1)) || usage ;;
    add) ((${#args[@]} == 3)) || usage ;;
    remove) ((${#args[@]} == 2)) || usage ;;
    *) usage ;;
esac
umask 077
work=$(mktemp -d)
trap 'rm -rf -- "${work:?}"' EXIT
kubectl -n "$ns" get secret "$secret" -o json > "$work/current.json"
python3 -I - "$cmd" "${args[1]:-}" "${args[2]:-}" "$work" <<'PY'
import base64, json, re, sys
cmd, node, digest, work = sys.argv[1:]
def pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            sys.exit('duplicate node name in token-hash file')
        result[key] = value
    return result
try:
    secret = json.load(open(work + '/current.json'))
    cur = json.loads(base64.b64decode(secret['data']['tokens.json'], validate=True), object_pairs_hook=pairs)
    if not isinstance(cur, dict):
        raise ValueError()
    if any(not re.fullmatch(r'[a-z][a-z0-9-]{0,31}', k) or not isinstance(v, str) or not re.fullmatch(r'[0-9a-f]{64}', v) for k, v in cur.items()):
        raise ValueError()
    if len(set(cur.values())) != len(cur):
        raise ValueError()
except (KeyError, ValueError, TypeError):
    sys.exit('current token-hash map is invalid')
if cmd == 'list':
    print('\n'.join(sorted(cur)))
    sys.exit(0)
if not re.fullmatch(r'[a-z][a-z0-9-]{0,31}', node):
    sys.exit('bad node name')
new = dict(cur)
if cmd == 'add':
    if not re.fullmatch(r'[0-9a-f]{64}', digest):
        sys.exit('hash must be a lowercase SHA-256 hex digest; raw tokens are not accepted')
    if node in cur:
        sys.exit('node already exists; remove it first to rotate its token')
    if digest in cur.values():
        sys.exit('token hash is already used by another node')
    new[node] = digest
else:
    if node not in cur:
        sys.exit('node is not a hub node')
    del new[node]
    if not new:
        sys.exit('refusing to remove the last node')
if not all(new[k] == cur[k] for k in cur if k != node):
    sys.exit('untouched entries changed')
# resourceVersion prevents overwriting another operator's concurrent update.
patch = {'metadata': {'resourceVersion': secret['metadata']['resourceVersion']},
         'data': {'tokens.json': base64.b64encode(json.dumps(new, indent=2).encode()).decode()}}
with open(work + '/patch.json', 'w') as output:
    json.dump(patch, output)
with open(work + '/expected.json', 'w') as output:
    json.dump(new, output)
PY
[[ "$cmd" == list ]] && exit 0
mkdir -p -- "$backup_dir"
backup=$(mktemp "$backup_dir/tokens-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX.json")
cp -- "$work/current.json" "$backup"
echo "backup: $backup"
kubectl -n "$ns" patch secret "$secret" --type merge --patch-file "$work/patch.json"
kubectl -n "$ns" get secret "$secret" -o json > "$work/after.json"
python3 -I - "$cmd" "${args[1]}" "$work" <<'PY'
import base64, json, sys
cmd, node, work = sys.argv[1:]
def load(name):
    with open(work + '/' + name + '.json') as source:
        return json.loads(base64.b64decode(json.load(source)['data']['tokens.json']))
before, after = load('current'), load('after')
with open(work + '/expected.json') as source:
    expected = json.load(source)
if not all(after.get(k) == v for k, v in before.items() if k != node):
    sys.exit('verification failed: untouched entries changed; backup kept')
if after != expected:
    sys.exit('verification failed: requested change not applied exactly; backup kept')
print(f'{cmd} {node}: verified; backup kept')
PY
echo 'The hub rereads the file per request; kubelet propagation can take up to approximately two minutes.'
echo 'Restore by applying the backup after removing metadata.resourceVersion.'
