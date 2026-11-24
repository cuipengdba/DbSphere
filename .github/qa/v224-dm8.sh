#!/usr/bin/env bash
set -euo pipefail
qa="$RUNNER_TEMP/dm8-v224"
mkdir -p "$qa"/{media,mount,install,data,trace}
url=https://download.dameng.com/eco/adapter/DM8/202607/dm8_20260710_x86_kylin10_sp3_64.zip
curl --fail --location --retry 2 --connect-timeout 15 --max-time 260 "$url" -o "$qa/media.zip"
unzip -qo "$qa/media.zip" -d "$qa/media"
iso="$(find "$qa/media" -type f -iname '*.iso' -print -quit)"
test -n "$iso"
sudo mount -o loop,ro "$iso" "$qa/mount"
trap 'sudo umount "$qa/mount"' EXIT
installer="$(find "$qa/mount" -type f -name 'DMInstall.bin' -print -quit)"
test -n "$installer"
cat >"$qa/install.xml" <<XML
<?xml version="1.0"?>
<DATABASE>
  <LANGUAGE>EN</LANGUAGE>
  <TIME_ZONE>+00:00</TIME_ZONE>
  <INSTALL_TYPE>0</INSTALL_TYPE>
  <INSTALL_PATH>$qa/install</INSTALL_PATH>
  <INIT_DB>N</INIT_DB>
  <CREATE_DB_SERVICE>N</CREATE_DB_SERVICE>
  <STARTUP_DB_SERVICE>N</STARTUP_DB_SERVICE>
</DATABASE>
XML
timeout 240 bash "$installer" -q "$qa/install.xml"
export DM_HOME="$qa/install"
export LD_LIBRARY_PATH="$DM_HOME/bin"
password='DbSphereFixture!128'
"$DM_HOME/bin/dminit" "PATH=$qa/data" CHARSET=1 LOG_SIZE=128 PORT_NUM=15236 \
  "SYSDBA_PWD=$password" "SYSAUDITOR_PWD=$password" >"$qa/init.log" 2>&1 || { cat "$qa/init.log"; exit 1; }
db="$qa/data/DAMENG"
test -f "$db/dm.ini"
sed -i -E 's/^(\s*SVR_LOG\s*=\s*)[^[:space:];]+/\11/' "$db/dm.ini"
cat >"$db/sqllog.ini" <<INI
BUF_TOTAL_SIZE = 10240
BUF_SIZE = 1024
BUF_KEEP_CNT = 6
[SLOG_ALL]
FILE_PATH = $qa/trace
PART_STOR = 0
SWITCH_MODE = 2
SWITCH_LIMIT = 100
ASYNC_FLUSH = 0
FILE_NUM = 3
ITEMS = 0
SQL_TRACE_MASK = 1
MIN_EXEC_TIME = 0
USER_MODE = 0
INI
"$DM_HOME/bin/dmserver" "$db/dm.ini" -noconsole >"$qa/server.log" 2>&1 &
engine=$!
trap 'kill "$engine" 2>/dev/null || true; sudo umount "$qa/mount"' EXIT
for ((attempt=0; attempt<45; attempt++)); do
  if grep -qi 'SYSTEM IS READY' "$qa/server.log"; then break; fi
  kill -0 "$engine" 2>/dev/null || { tail -n 40 "$qa/server.log"; exit 1; }
  sleep 1
done
grep -qi 'SYSTEM IS READY' "$qa/server.log" || { tail -n 40 "$qa/server.log"; exit 1; }
sql() {
  "$DM_HOME/bin/disql" "SYSDBA/$password@127.0.0.1:15236"
}
sql >"$qa/sql.out" <<'SQL'
SELECT * FROM V$VERSION;
CALL SP_SET_PARA_VALUE(1, 'SVR_LOG', 1);
CALL SP_REFRESH_SVR_LOG_CONFIG();
CREATE TABLE DBS_V224_ACCEPT (ID INT, NOTE VARCHAR(50));
INSERT INTO DBS_V224_ACCEPT VALUES (1, 'dbsphere-dm8-fixture-literal');
COMMIT;
SELECT NOTE FROM DBS_V224_ACCEPT WHERE NOTE = 'dbsphere-dm8-fixture-literal';
SELECT MISSING_COLUMN FROM DBS_V224_ACCEPT;
exit
SQL
grep -E 'DM Database|DB Version|DM8|V8|8\.1\.3\.' "$qa/sql.out" | head -n 8 || true
grep -q 'dbsphere-dm8-fixture-literal' "$qa/sql.out"
for ((attempt=0; attempt<15; attempt++)); do
  log="$(find "$qa/trace" "$db" "$DM_HOME/log" -iname 'dmsql*.log' -type f -size +0c -print 2>/dev/null | head -n 1 || true)"
  if [[ -n "$log" ]] && grep -qi 'DBS_V224_ACCEPT' "$log"; then break; fi
  sleep 1
done
test -n "$log"
grep -qi 'DBS_V224_ACCEPT' "$log"
node .github/qa/v224-vendor.mjs dm8 "$log"
