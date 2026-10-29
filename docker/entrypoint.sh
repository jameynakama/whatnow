#!/bin/sh
set -eu
umask 077

password_file=/run/secrets/keyring_password
if [ ! -r "$password_file" ] || [ ! -s "$password_file" ]; then
    echo "A readable, nonempty /run/secrets/keyring_password file is required. See docs/docker.md." >&2
    exit 1
fi

mkdir -p "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME/keyrings"
chmod 700 "$XDG_RUNTIME_DIR" "$XDG_DATA_HOME/keyrings"

#* Keep the password out of command arguments and the environment.
gnome-keyring-daemon --unlock --components=secrets --control-directory="$XDG_RUNTIME_DIR/keyring" < "$password_file"

#* A wrong password can leave the daemon running with a locked collection.
if ! dbus-send --session --print-reply --dest=org.freedesktop.secrets \
    /org/freedesktop/secrets/collection/login \
    org.freedesktop.DBus.Properties.Get \
    string:org.freedesktop.Secret.Collection string:Locked | grep -q 'boolean false'; then
    echo "Could not unlock the login keyring. Restore its original password and restart." >&2
    exit 1
fi

exec "$@"
