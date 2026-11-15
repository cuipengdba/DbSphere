param([int[]]$Versions = @(14,15,16,17,18))

$ErrorActionPreference = 'Stop'
$failed = $false
try {
  foreach ($version in $Versions) {
    $name = 'dbsphere-pgtest-{0}' -f $version
    $image = 'postgres:{0}-bookworm' -f $version
    $dataPath = if ($version -ge 18) { '/var/lib/postgresql' } else { '/var/lib/postgresql/data' }
    $tmpfs = '{0}:rw,size=256m' -f $dataPath
    docker run -d --rm --name $name -e POSTGRES_PASSWORD=dbsphere-root-test -p 127.0.0.1:15432:5432 --tmpfs $tmpfs $image | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to start PG$version" }
    $ready = $false
    for ($i = 0; $i -lt 60; $i++) {
      docker exec $name pg_isready -h 127.0.0.1 -U postgres 2>$null | Out-Null
      if ($LASTEXITCODE -eq 0) { $ready = $true; break }
      Start-Sleep -Seconds 1
    }
    if (-not $ready) { throw "PG$version did not become ready" }
    docker exec -e PGPASSWORD=dbsphere-root-test $name psql -h 127.0.0.1 -U postgres -c "CREATE ROLE dbsphere_monitor LOGIN PASSWORD 'dbsphere-monitor-test'; GRANT pg_monitor TO dbsphere_monitor;" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to create PG$version monitor role" }
    $env:DBSPHERE_TEST_PG_MAJOR = [string]$version
    node scripts/pg-compat.mjs
    if ($LASTEXITCODE -ne 0) { throw "PG$version compatibility test failed" }
    docker stop $name | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to stop PG$version" }
    Start-Sleep -Seconds 1
  }
} catch {
  Write-Error $_
  $failed = $true
} finally {
  foreach ($version in $Versions) {
    $name = 'dbsphere-pgtest-{0}' -f $version
    $running = docker ps -q --filter "name=^/$name$"
    if ($running) { docker stop $name | Out-Null }
  }
}
if ($failed) { exit 1 }
