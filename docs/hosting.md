# Hosting whatnow

How to run `whatnow host` for other people on a DigitalOcean droplet, with Caddy for HTTPS, user
data on a DigitalOcean Volume, and continuous backups to Cloudflare R2. It assumes you have never
made a Volume or an R2 bucket. Do the sections in order.

What you end up with:

- A droplet running `whatnow host` under systemd, listening only on `127.0.0.1:8080`.
- Caddy in front, doing HTTPS for your domain.
- Every database on a Volume mounted at `/mnt/whatnow`: `accounts.db`, and one `users/<id>.db` per
  person, a new file each time someone signs up. Rebuilding the droplet doesn't touch the Volume.
- Litestream copying every one of those files to R2 continuously, including files that appear
  after it starts, and keeping 30 days of history.

Throughout, `whatnow.example.com` is your domain. Replace it everywhere.

## 1. Create the droplet and the Volume

1. In the DigitalOcean control panel, choose **Create > Droplets**. Pick Ubuntu 24.04 LTS, the
   smallest Basic plan, and a region. Add your SSH key. Create it.
2. Choose **Create > Volumes**. Make it 10 GiB (it can be grown later), in the **same region**, and
   attach it to the droplet. When asked, choose **Manually Format & Mount**.
3. Under **Networking > Firewalls**, create a cloud firewall that allows inbound **22, 80, and 443**
   only, and apply it to the droplet.
4. Point your domain at the droplet: add an `A` record for `whatnow.example.com` with the droplet's
   public IP.

The rest of this guide runs on the droplet: `ssh root@whatnow.example.com`.

Keep every other service on this droplet bound to loopback (`127.0.0.1`), never `0.0.0.0`. whatnow
refuses to fetch private addresses on a user's behalf, but it can still reach the droplet's own
public IP, so anything listening there could be reached through it. Run nothing else here if you
can.

## 2. Mount the Volume at /mnt/whatnow

Find the Volume's device:

```sh
ls /dev/disk/by-id/ | grep scsi-0DO_Volume
```

You'll see a name like `scsi-0DO_Volume_volume-nyc1-01`. Format it once (this erases it, so only
ever do this on a new, empty Volume), mount it, and make the mount survive reboots:

```sh
DEV=/dev/disk/by-id/scsi-0DO_Volume_volume-nyc1-01   # use the name you saw
mkfs.ext4 "$DEV"
mkdir -p /mnt/whatnow
echo "$DEV /mnt/whatnow ext4 defaults,nofail,discard 0 2" >> /etc/fstab
mount -a
df -h /mnt/whatnow
```

`df` should show the Volume's size. `nofail` lets the droplet boot even if the Volume is missing.

## 3. Create the whatnow user

```sh
useradd --system --home /mnt/whatnow --shell /usr/sbin/nologin whatnow
chown whatnow:whatnow /mnt/whatnow
chmod 700 /mnt/whatnow
```

## 4. Install Caddy

```sh
apt update
apt install -y debian-keyring debian-archive-keyring apt-transport-https curl gpg sqlite3
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
apt update
apt install -y caddy
```

If those lines have changed, the current ones are at <https://caddyserver.com/docs/install>.

Replace `/etc/caddy/Caddyfile` with:

```
whatnow.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

```sh
systemctl reload caddy
```

Caddy fetches and renews the HTTPS certificate on its own, as long as the `A` record from section
1 has propagated and ports 80 and 443 are open.

## 5. Create the Google OAuth client

whatnow signs people in with Google.

1. Go to <https://console.cloud.google.com/> and create a project (or pick one).
2. **APIs & Services > OAuth consent screen**: choose **External**, fill in the app name, support
   email, and your domain. Scopes: leave the defaults (`openid`, `email`, `profile`). Publish the
   app to **In production**, or only listed test users can sign in.
3. **APIs & Services > Credentials > Create credentials > OAuth client ID**. Application type:
   **Web application**. Under **Authorized redirect URIs**, add exactly:

   ```
   https://whatnow.example.com/auth/google/callback
   ```

4. Copy the **Client ID** and **Client secret**. You need them in section 6.

## 6. Generate the seal key and write the environment file

whatnow encrypts each user's secrets (model API keys, and integration secrets such as calendar
addresses and Jira and Linear tokens) with a seal key before storing them in their database. Each account's
Google name and email are stored in `accounts.db` unencrypted, so the app can show who is signed
in; deleting the account removes them, and older backup copies of `accounts.db` age out within 30
days. Generate the key on your own machine or the droplet:

```sh
openssl rand -base64 32
```

The key lives in `/etc/whatnow.env` on the droplet's own disk, which the server needs in order to
decrypt secrets while it runs. **Also keep a second copy somewhere safe that is not this server**,
such as your password manager:

- The Volume is what gets backed up, and the key is deliberately not on it. A stolen or leaked
  backup holds encrypted secrets but no key, so it can't decrypt anyone's secrets.
- The flip side: if you lose the key, the encrypted secrets in the databases (and in every backup)
  are unreadable, and every user has to enter their API key and integration secrets again. A restore onto a new droplet needs
  the same key.

Write the environment file on the droplet, readable by root only:

```sh
install -m 600 -o root -g root /dev/null /etc/whatnow.env
cat > /etc/whatnow.env <<'ENV'
WHATNOW_GOOGLE_CLIENT_ID=paste-the-client-id
WHATNOW_GOOGLE_CLIENT_SECRET=paste-the-client-secret
WHATNOW_SEAL_KEY=paste-the-output-of-openssl
ENV
ls -l /etc/whatnow.env   # -rw------- root root
```

systemd reads this file as root and hands the values to the service, so the `whatnow` user never
needs to read it.

Leave every `ANTHROPIC_*` and `OPENAI_*` variable out of this file and the unit. The model SDKs
would read them as defaults for every user's requests, so `whatnow host` refuses to start while any
is set.

## 7. Install the binary and the systemd unit

On your own machine, from the repo:

```sh
just build-linux
scp dist/whatnow-linux-amd64 root@whatnow.example.com:/usr/local/bin/whatnow
```

On the droplet:

```sh
chmod 755 /usr/local/bin/whatnow
cat > /etc/systemd/system/whatnow.service <<'UNIT'
[Unit]
Description=whatnow (hosted)
After=network-online.target mnt-whatnow.mount
Wants=network-online.target
RequiresMountsFor=/mnt/whatnow

[Service]
User=whatnow
Group=whatnow
EnvironmentFile=/etc/whatnow.env
ExecStart=/usr/local/bin/whatnow host -addr 127.0.0.1:8080 -data /mnt/whatnow -public-url https://whatnow.example.com -max-users 100
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/mnt/whatnow
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now whatnow
systemctl status whatnow
journalctl -u whatnow -f
```

`-max-users` is the sign-up cap (default 100); change it here and restart to raise it.
`just users` prints how many accounts exist, and each turned-away sign-up logs `sign-up refused`
to the journal.

Visit `https://whatnow.example.com` and sign in with Google to check the whole chain. Because the
Volume is mounted with `nofail`, `RequiresMountsFor` keeps the service from starting against an
empty directory if the mount ever fails.

## 8. Create the R2 bucket and an API token

1. Sign in at <https://dash.cloudflare.com/> and open **R2 Object Storage** in the sidebar. The
   first time, you must add a payment method; R2's free tier is generous, and there is no charge for
   downloads out of R2.
2. **Create bucket**. Name it `whatnow-backups`. Leave the location on Automatic (or pick the
   region nearest your droplet).
3. On the R2 overview page, copy your **Account ID** (right-hand side).
4. **Manage R2 API Tokens > Create API token**. Permission: **Object Read & Write**. Under
   **Specify bucket(s)**, choose `whatnow-backups` only. Create it.
5. Copy the **Access Key ID** and **Secret Access Key** now; the secret is shown once.

## 9. Install Litestream and configure backups

[Litestream](https://litestream.io/) watches a SQLite database and streams every change to object
storage. Pointed at a directory with `watch: true`, it picks up new database files within seconds,
so each new sign-up's file is backed up too.

Install it (0.5.x; check <https://github.com/benbjohnson/litestream/releases> for the latest):

```sh
cd /tmp
curl -LO https://github.com/benbjohnson/litestream/releases/download/v0.5.17/litestream-0.5.17-linux-x86_64.deb
dpkg -i litestream-0.5.17-linux-x86_64.deb
```

Write the config, readable by root only because it holds the R2 keys:

```sh
install -m 600 -o root -g root /dev/null /etc/litestream.yml
cat > /etc/litestream.yml <<'YAML'
# Keep 30 days: a snapshot a day, and each snapshot (with the changes since it) lives 720 hours.
snapshot:
  interval: 24h
  retention: 720h

# Litestream polls R2 on these timers for every database, even idle ones, and each poll is a
# billable Class A operation. The defaults (L1 every 30s, L0 check every 15s) cost about
# 9k operations per database per day, which leaves the free tier behind after a few dozen users.
levels:
  - interval: 5m
  - interval: 1h
l0-retention-check-interval: 5m

dbs:
  - dir: /mnt/whatnow
    pattern: "*.db"
    recursive: true
    watch: true
    replica:
      type: s3
      # A dead Volume loses at most this much of each user's latest writes. The default is 1s.
      sync-interval: 1m
      bucket: whatnow-backups
      path: whatnow
      endpoint: https://YOUR-ACCOUNT-ID.r2.cloudflarestorage.com
      region: auto
      access-key-id: PASTE-ACCESS-KEY-ID
      secret-access-key: PASTE-SECRET-ACCESS-KEY
YAML
systemctl enable --now litestream
systemctl status litestream
journalctl -u litestream -n 50
```

The journal should show `accounts.db` and each existing `users/*.db` being replicated. Sign in with
a second Google account, then check the journal again: a new `users/<id>.db` should appear within
seconds. In the Cloudflare dashboard, the bucket now holds `whatnow/accounts.db/...` and
`whatnow/users/<id>.db/...`.

Things to know:

- The bucket layout mirrors the directory, so `/mnt/whatnow/users/abc.db` lives under
  `whatnow/users/abc.db/`.
- When a user deletes their account, Litestream stops replicating their file, but their existing
  backup stays in R2 until you remove it. Section 10 sets up a daily sweep that does that. Don't
  skip it: the app tells users their backups are gone within 30 days.
- Litestream only replicates files with a valid SQLite header, so stray files matching `*.db` are
  ignored.

## 10. Sweep deleted users' backups

Litestream's 30-day retention only trims databases it is still replicating. When someone deletes
their account, their file leaves the Volume, Litestream stops, and their backup in R2 would sit
there forever. whatnow tells users a deleted account's backups are gone within 30 days, so a daily
sweep removes the backup of every user whose `users/<id>.db` is no longer on the Volume. It also
removes the `users/.<id>.db-litestream` directory Litestream keeps beside each database, which holds
recent changes and stays behind after the database is deleted. That happens within a day of the
deletion; the deleted user's rows in older `accounts.db` history age out with the 30-day retention.
Don't use an R2 lifecycle rule for this instead: it can't tell a deleted user from one who is just
idle, and it would expire the base snapshots that idle users' backups need.

Make a second R2 API token for the sweep, the same way as in section 8 (**Object Read & Write**,
this bucket only), and name it `whatnow-sweep`. Then install [rclone](https://rclone.org/) (1.59 or
newer, which the install script gives you; the Ubuntu package is older):

```sh
curl https://rclone.org/install.sh | bash
install -m 600 -o root -g root /dev/null /etc/whatnow-sweep.rclone.conf
cat > /etc/whatnow-sweep.rclone.conf <<'CONF'
[r2]
type = s3
provider = Cloudflare
access_key_id = PASTE-SWEEP-ACCESS-KEY-ID
secret_access_key = PASTE-SWEEP-SECRET-ACCESS-KEY
endpoint = https://YOUR-ACCOUNT-ID.r2.cloudflarestorage.com
acl = private
no_check_bucket = true
CONF
```

Write the script:

```sh
cat > /usr/local/bin/whatnow-backup-sweep <<'SCRIPT'
#!/usr/bin/env bash
# Delete the R2 backup, and the local Litestream state, of every user whose database is gone from
# the Volume.
#   --dry-run  print what would be deleted
#   --force    skip the "too many at once" check, after you've read what it would delete
set -euo pipefail

DATA=/mnt/whatnow
REMOTE=r2:whatnow-backups/whatnow/users
MAX_DELETES=5
rc=(rclone --config /etc/whatnow-sweep.rclone.conf)
dry=()
force=false
for arg in "$@"; do
  case $arg in
    --dry-run) dry=(--dry-run) ;;
    --force) force=true ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

refuse() { echo "refusing to sweep: $*" >&2; exit 1; }

# An unmounted or empty Volume must never look like "every user was deleted".
mountpoint -q "$DATA" || refuse "$DATA is not a mounted Volume"
[[ -f $DATA/accounts.db ]] || refuse "$DATA/accounts.db is missing"
[[ -n $(find "$DATA/users" -maxdepth 1 -name '*.db' -print -quit 2>/dev/null) ]] \
  || refuse "$DATA/users has no databases"

listing=$("${rc[@]}" lsf --dirs-only "$REMOTE")   # one "<id>.db/" per backed-up user
backed_up=()
stale=()
while IFS= read -r prefix; do
  [[ -n $prefix ]] || continue
  name=${prefix%/}
  backed_up+=("$name")
  [[ -e $DATA/users/$name ]] || stale+=("$name")
done <<<"$listing"

# Litestream keeps each database's recent changes in a hidden .<id>.db-litestream directory beside
# it, and leaves that behind when the database is deleted.
leftover=()
for meta in "$DATA"/users/.*.db-litestream; do
  [[ -d $meta ]] || continue
  name=${meta##*/.}
  [[ -e $DATA/users/${name%-litestream} ]] || leftover+=("$meta")
done

echo "${#backed_up[@]} backed up, ${#stale[@]} to delete, ${#leftover[@]} local leftovers"
((${#stale[@]} + ${#leftover[@]} > 0)) || exit 0

if [[ $force != true ]] && ((${#stale[@]} > MAX_DELETES || ${#stale[@]} * 2 > ${#backed_up[@]} \
  || ${#leftover[@]} > MAX_DELETES)); then
  printf 'would delete: %s\n' "${stale[@]}" "${leftover[@]}" >&2
  refuse "that is too many at once; check the Volume, then rerun with --force"
fi

for name in "${stale[@]}"; do
  echo "deleting $name"
  "${rc[@]}" purge "${dry[@]}" "$REMOTE/$name"
done
for meta in "${leftover[@]}"; do
  echo "deleting $meta"
  ((${#dry[@]})) || rm -rf -- "$meta"
done
SCRIPT
chmod 755 /usr/local/bin/whatnow-backup-sweep
```

The checks mean a missing Volume, an empty `users/` directory, or a run that would delete more than
5 backups (or more than half of them) or more than 5 local leftovers does nothing and exits with an
error. Early on, with only a few users, one deletion can trip the "more than half" check; that's
expected. Read the output and rerun with `--force` if it looks right.

Try it with `--dry-run`, which prints what it would delete and deletes nothing:

```sh
whatnow-backup-sweep --dry-run
```

With no deleted accounts it prints `N backed up, 0 to delete, 0 local leftovers`. Then run it
daily with a systemd timer:

```sh
cat > /etc/systemd/system/whatnow-backup-sweep.service <<'UNIT'
[Unit]
Description=Delete backups of deleted whatnow accounts
RequiresMountsFor=/mnt/whatnow

[Service]
Type=oneshot
ExecStart=/usr/local/bin/whatnow-backup-sweep
UNIT
cat > /etc/systemd/system/whatnow-backup-sweep.timer <<'UNIT'
[Unit]
Description=Daily sweep of deleted whatnow accounts' backups

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now whatnow-backup-sweep.timer
systemctl list-timers whatnow-backup-sweep.timer
```

A refused or failed run shows up as a failed unit: check `systemctl status whatnow-backup-sweep`
now and then, and read `journalctl -u whatnow-backup-sweep`. A deleting run logs
`ERROR ... GetBucketVersioning ... AccessDenied`: rclone asks whether the bucket keeps old
versions, the bucket-scoped token isn't allowed to ask, and rclone carries on assuming it doesn't
(R2's default). It is harmless; don't widen the token to silence it.

## 11. The restore drill

Do this once before you open to users, and again whenever you change the setup. It proves the
backups work and that you can read them without the original server.

On a scratch machine (a throwaway droplet, or any Linux box with `sqlite3`), install the same
Litestream version as the server (v0.5.17 above), then:

```sh
export LITESTREAM_ACCESS_KEY_ID=PASTE-ACCESS-KEY-ID
export LITESTREAM_SECRET_ACCESS_KEY=PASTE-SECRET-ACCESS-KEY
R2='endpoint=https://YOUR-ACCOUNT-ID.r2.cloudflarestorage.com&region=auto'
mkdir -p /tmp/restore/users

litestream restore -o /tmp/restore/accounts.db "s3://whatnow-backups/whatnow/accounts.db?$R2"
sqlite3 /tmp/restore/accounts.db 'select id, email, created_at from users;'

ID=pick-an-id-from-the-list-above
litestream restore -o /tmp/restore/users/$ID.db "s3://whatnow-backups/whatnow/users/$ID.db?$R2"
sqlite3 /tmp/restore/users/$ID.db '.tables' 'select count(*) from tasks;'
```

If both open and the rows look right, the drill passes. To restore to an earlier moment, add
`-timestamp YYYY-MM-DDTHH:MM:SSZ` (a UTC time within the last 30 days) before the URL.

The drill also covers the sweep from section 10, and proves a deleted account leaves nothing behind.
On the server:

1. Sign in with a throwaway Google account and add a task. Its database is the newest file in
   `ls -lt /mnt/whatnow/users/`; set `ID=` to its name without `.db`. Wait a minute so Litestream
   backs it up.
2. In Settings, delete that account.
3. Run `whatnow-backup-sweep --dry-run`. It should name only the throwaway's `<id>.db` and its
   `.<id>.db-litestream` directory (plus any users you know have deleted their accounts), and say
   nothing is wrong with the Volume.
4. Run it for real, `whatnow-backup-sweep`, adding `--force` if the "more than half" check trips with
   few users. This proves the sweep's R2 token can delete.
5. Check that nothing of the throwaway remains:

   ```sh
   ls -la /mnt/whatnow/users/ | grep "$ID"
   rclone --config /etc/whatnow-sweep.rclone.conf lsf r2:whatnow-backups/whatnow/users/ | grep "$ID"
   ```

   Both should print nothing.

Litestream can't restore every database in one command when it replicates a directory. To rebuild
a whole server, list the users from the restored `accounts.db` and restore `users/<id>.db` for each.
Put the files at `/mnt/whatnow/accounts.db` and `/mnt/whatnow/users/<id>.db`, owned by `whatnow`,
set `WHATNOW_SEAL_KEY` to your saved copy, and start the service. Stop `litestream` on the new
server until the restore is done so it doesn't start replicating partial files.

## 12. Deploy an update

On your own machine:

```sh
just deploy root@whatnow.example.com
```

It runs the tests, builds the Linux binary, copies it up, restarts the service, and checks the site answers. If the
service doesn't come back, it prints the last lines of `journalctl -u whatnow` and fails. Users stay
signed in across a restart, because sessions live in `accounts.db`.
