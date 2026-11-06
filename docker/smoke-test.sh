#!/bin/sh
set -eu
umask 077

cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
name="whatnow-smoke-$(basename "$scratch")"
volume="$name-data"
restored="$name-restored"
image=whatnow-smoke:local

cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker volume rm "$volume" "$restored" >/dev/null 2>&1 || true
    rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'smoke-test-password' > "$scratch/password"
printf 'incorrect-password' > "$scratch/wrong"
: > "$scratch/empty"
chmod 444 "$scratch/password" "$scratch/wrong" "$scratch/empty"
docker build -t "$image" .
docker volume create "$volume" >/dev/null
docker volume create "$restored" >/dev/null

start() {
    docker run -d --name "$name" --read-only --tmpfs /tmp:mode=1777 \
        --cap-drop ALL --security-opt no-new-privileges:true \
        --mount "type=volume,src=$1,dst=/data" \
        --mount "type=bind,src=$scratch/password,dst=/run/secrets/keyring_password,readonly" \
        "$image" >/dev/null
    attempt=0
    until api http://127.0.0.1:7777/api/v1/settings >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 30 ] || [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then
            docker logs "$name"
            return 1
        fi
        sleep 1
    done
}

api() {
    docker exec "$name" curl -fsS --max-time 5 "$@"
}

check_saved() {
    api http://127.0.0.1:7777/api/v1/tasks | grep -q 'Docker smoke task'
    api http://127.0.0.1:7777/api/v1/settings | grep -q '"openai":"keychain"'
}

start "$volume"
api http://127.0.0.1:7777/ | grep -q '<html'
api -X POST -H 'Content-Type: application/json' -d '{"title":"Docker smoke task"}' \
    http://127.0.0.1:7777/api/v1/tasks >/dev/null
api -X PUT -H 'Content-Type: application/json' -d '{"key":"smoke-dummy-key"}' \
    http://127.0.0.1:7777/api/v1/settings/keys/openai
check_saved
docker stop "$name" >/dev/null
docker rm "$name" >/dev/null
start "$volume"
check_saved
echo 'PASS: UI, API, and database/keyring persistence across container replacement'

docker stop "$name" >/dev/null
docker rm "$name" >/dev/null
docker run --rm --read-only --mount "type=volume,src=$volume,dst=/data,readonly" \
    --entrypoint tar "$image" -C /data -czf - . > "$scratch/backup.tar.gz"
docker run --rm -i --read-only --mount "type=volume,src=$restored,dst=/data" \
    --entrypoint tar "$image" -C /data -xzf - < "$scratch/backup.tar.gz"
start "$restored"
check_saved
docker stop "$name" >/dev/null
docker rm "$name" >/dev/null
echo 'PASS: database and keyring restored with the original password'

for password in wrong empty missing; do
    set --
    if [ "$password" != missing ]; then
        set -- --mount "type=bind,src=$scratch/$password,dst=/run/secrets/keyring_password,readonly"
    fi
    docker create --name "$name" --read-only --tmpfs /tmp:mode=1777 \
        --cap-drop ALL --security-opt no-new-privileges:true \
        --mount "type=volume,src=$restored,dst=/data" "$@" "$image" >/dev/null
    docker start "$name" >/dev/null
    attempt=0
    while [ "$(docker inspect -f '{{.State.Running}}' "$name")" = true ]; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 30 ]; then
            docker logs "$name"
            echo "FAIL: startup did not reject $password password" >&2
            exit 1
        fi
        sleep 1
    done
    test "$(docker inspect -f '{{.State.ExitCode}}' "$name")" -ne 0
    if [ "$password" = wrong ]; then
        docker logs "$name" 2>&1 | grep -q 'Could not unlock the login keyring'
    else
        docker logs "$name" 2>&1 | grep -q 'A readable, nonempty /run/secrets/keyring_password file is required'
    fi
    docker rm "$name" >/dev/null
    echo "PASS: $password password rejected"
done
