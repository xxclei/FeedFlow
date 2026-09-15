@echo off
setlocal enabledelayedexpansion

REM ===========================================================================
REM  myfeed load-test harness  (stage 7 cache ledger)
REM
REM  USAGE
REM    scripts\bench.bat                       same as: auth 8000 80
REM    scripts\bench.bat <mode> [n] [c] [label]
REM    scripts\bench.bat all  [n] [c] [label]  run every mode, print summary
REM    scripts\bench.bat ab   [n] [c] [label]  A/B sweep: cache ON vs cache OFF
REM    scripts\bench.bat list                  show all modes
REM    scripts\bench.bat ledger                dump the accumulated CSV
REM
REM  EXAMPLES
REM    scripts\bench.bat video 8000 80 before-video-cache
REM    scripts\bench.bat video 8000 80 after-video-cache
REM    scripts\bench.bat all   8000 80 stage7-step2
REM    scripts\bench.bat ab    8000 80 stage7-step2     <- the money shot
REM
REM  `ab` IN ONE PARAGRAPH
REM    Runs the whole 16-mode sweep TWICE and tags every row with the `redis`
REM    column. Group 1 has Redis up; group 2 stops the container AND restarts
REM    the server, so the app boots with cache==nil. That second form is the
REM    honest "this system was never given a cache" baseline -- stopping the
REM    container without restarting would instead measure "Redis is dead",
REM    which also pays a failed-dial penalty on every single request and is a
REM    worst case, not a baseline. The two differ by a lot; do not mix them up.
REM
REM  TWO EVIDENCE SOURCES (independent -- always check both)
REM    1) hey    -> QPS + latency percentiles + status codes
REM    2) mysql  -> Com_stmt_execute delta = SQL statements actually run
REM
REM  WHY Com_stmt_execute AND NOT Questions
REM    The Go MySQL driver sends every parameterized query as a prepared
REM    statement. `Questions` also counts connection handshakes and
REM    Com_stmt_prepare, so it reports nonsense like 1.84 per request for a
REM    path that truly runs exactly 1. Com_stmt_execute counts real
REM    executions only -> always an exact integer. If you ever see a
REM    fractional ratio, the counter is wrong, not the code.
REM
REM  WHY EVERY MODE RECORDS ITS ROUTE
REM    A cache only helps if the benchmark actually passes through the layer
REM    you changed. /video/getDetail is registered in the PUBLIC video group
REM    (no JWTAuth), so it can NEVER measure the JWT cache. Recording the
REM    route in the CSV makes that mistake visible instead of mysterious.
REM
REM  PORT EXHAUSTION -- THE #1 WAY TO GET A GARBAGE NUMBER HERE
REM    Windows has ~16384 dynamic ports and every closed TCP connection
REM    parks in TIME_WAIT for ~120s. Symptoms:
REM      - hey reports [500] on the heavier endpoints (the app's own DB dial
REM        fails with "Only one usage of each socket address")
REM      - curl returns status 000 (the *client* cannot get a port either)
REM      - QPS collapses to a few hundred for no apparent reason
REM    So: run `netstat -an | find /c "TIME_WAIT"` before trusting a number.
REM    The preflight below warns. If it is high, wait 2 minutes and rerun.
REM
REM    A leftover hey.exe is worse than high TIME_WAIT, because it keeps
REM    re-creating sockets forever: it hammers the server while your next
REM    measurement runs, so that measurement is fiction. The preflight kills
REM    any orphan. (Note: `pkill` from git-bash does NOT kill it -- use
REM    taskkill /F /IM hey.exe.)
REM
REM  A/B RECIPE
REM    docker stop myfeed-redis    -> rerun -> see degraded numbers
REM    docker start myfeed-redis   -> rerun -> confirm recovery
REM    Cache-down also pays a dead-Redis dial penalty, so it is a WORST
REM    CASE, not a pure "no cache" baseline. For the pure baseline, stop
REM    Redis and restart the server: cache becomes nil at boot.
REM ===========================================================================

set "TOKEN=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhY2NvdW50X2lkIjoxLCJ1c2VybmFtZSI6ImFmIiwiZXhwIjoxNzg5NDQ5Nzc3LCJuYmYiOjE3ODkzNjMzNzcsImlhdCI6MTc4OTM2MzM3N30.4JX3tt-Z9QozlC93aEKd_l8uwItVIOjNlBofzR1rdVU"
set "BASE=http://127.0.0.1:8080"
set "MYSQL_CTR=docker exec myfeed-mysql mysql -uroot -p123456 -N -e"
set "SCRIPTS=%~dp0"
set "ROOT=%SCRIPTS%.."
set "EXE=%ROOT%\.run\myfeed.exe"
set "REDIS_CTR=myfeed-redis"
set "RCLI=docker exec myfeed-redis redis-cli -a 123456 --no-auth-warning"
REM  Which cache state the CURRENT group is measuring. Goes into the CSV so a
REM  row is never ambiguous about whether Redis was in the path. Defaults to
REM  "on" because that is what a plain single-mode run normally means.
set "REDISSTATE=on"
REM  Ledger of every run. Rows accumulate so you can diff before/after by label.
REM  (A stale `bench-ledger.csv` from an earlier session can end up locked by
REM  an antivirus/indexer scan -- harmless, but it is why this file has a
REM  different name than it originally did.)
set "CSV=%SCRIPTS%bench-results.csv"
set "LOG=%TEMP%\bench-hey.txt"

set "MODE=%~1"
if "%MODE%"=="" set "MODE=auth"
set "N=%~2"
if "%N%"=="" set "N=8000"
set "C=%~3"
if "%C%"=="" set "C=80"
set "LABEL=%~4"
if "%LABEL%"=="" set "LABEL=unlabeled"

if /i "%MODE%"=="list"   goto :list_modes
if /i "%MODE%"=="ledger" goto :show_ledger
if /i "%MODE%"=="ab"     goto :run_ab

REM  `all` expands to the full sweep. Order matters: cheapest/most diagnostic
REM  first, so a sweep that dies halfway still produced the useful rows.
set "ALL_MODES=health auth video author latest popular bytag likescount search following comments myliked followers vloggers counts profile"
if /i "%MODE%"=="all" ( set "MODES=!ALL_MODES!" ) else ( set "MODES=%MODE%" )
goto :run_all

REM ===========================================================================
REM  :run_ab  --  the same sweep twice: cache ON, then cache OFF
REM
REM  Group 2 is built as "stop redis, THEN restart the server" on purpose.
REM  Order matters: if you restart the server first and stop Redis after, the
REM  app passes its startup Ping and keeps a live-but-broken client -- that is
REM  the OTHER degradation path (request-level), which costs an extra failed
REM  dial on every request. Restarting after the stop makes Ping fail at boot,
REM  so main.go sets cache=nil and the whole group measures a clean no-cache
REM  system.
REM ===========================================================================
:run_ab
set "MODES=%ALL_MODES%"
echo.
echo ###########################################################################
echo   A/B SWEEP   n=%N%  c=%C%  label=%LABEL%
echo   group 1/2 : redis ON
echo   group 2/2 : redis OFF  (container stopped AND server restarted -^> cache=nil)
echo ###########################################################################

set "REDISSTATE=on"
call :start_redis
if errorlevel 1 goto :ab_abort
call :restart_server
if errorlevel 1 goto :ab_abort
call :flush_cache
call :sweep

set "REDISSTATE=off"
call :stop_redis
call :restart_server
if errorlevel 1 goto :ab_abort
call :sweep

REM  Put the system back the way we found it -- leaving Redis down after an A/B
REM  run would silently poison whatever the user measures next.
echo.
echo   [restore] bringing redis back up and restarting the server
set "REDISSTATE=on"
call :start_redis
call :restart_server
call :flush_cache
call :print_summary
goto :done

REM  Aborting mid-AB is the one case where we must still restore Redis: the
REM  user is about to go do something else, and a stopped container would look
REM  like "the cache silently stopped working".
:ab_abort
echo.
echo   [ABORT] a setup step failed. Restoring redis and the server, then stopping.
call :start_redis
call :restart_server
goto :done

REM  :sweep -- run every mode in MODES, with a port-exhaustion gate between them
REM
REM  NOTE THE SHAPE: `for ... do call :sweep_one %%M`, deliberately WITHOUT a
REM  parenthesised block. Writing it as
REM      for %%M in (!MODES!) do ( call :wait_ports & call :run_one %%M )
REM  hangs: every label here ends in `goto :eof`, and a `goto` executed inside
REM  a parenthesised block (or in a routine called from one) makes cmd discard
REM  the block context -- the loop never advances and no work happens. Sweep
REM  silently produced zero rows before this was restructured. When mixing
REM  `call` + `goto :eof` + FOR, keep the FOR body to a single bare command.
:sweep
echo.
echo ###########################################################################
echo   GROUP redis=%REDISSTATE%
echo ###########################################################################
for %%M in (!MODES!) do call :sweep_one %%M
goto :eof

:sweep_one
call :wait_ports
call :run_one %1
goto :eof

REM ===========================================================================
REM  :resolve  --  mode name -> METHOD / URL / BODY / AUTH / BASELINE
REM  AUTH=jwt  -> route sits behind JWTAuth, so the JWT cache is in the path
REM  AUTH=none -> public route; the JWT cache is NOT exercised
REM  Sets RESOLVE_ERR on unknown mode. (Do NOT `exit /b` here: exiting from a
REM  subroutine called inside a FOR loop aborts the whole loop in cmd.)
REM ===========================================================================
:resolve
set "RM=%~1"
set "METHOD=POST"
set "AUTH=jwt"
set "BASELINE="
set "RESOLVE_ERR="
if /i "%RM%"=="health"     ( set "METHOD=GET" & set "AUTH=none" & set "URL=%BASE%/healthz" & set "BODY=" & set "BASELINE=0 SQL -- pure HTTP ceiling, no DB no cache" & goto :eof )
if /i "%RM%"=="auth"       ( set "URL=%BASE%/like/isLiked" & set "BODY={\"video_id\":31}" & set "BASELINE=1 SQL -- JWT cache DONE (stage7 step1)" & goto :eof )
if /i "%RM%"=="video"      ( set "AUTH=none" & set "URL=%BASE%/video/getDetail" & set "BODY={\"id\":177}" & set "BASELINE=1 SQL -- target of stage7 step2" & goto :eof )
if /i "%RM%"=="author"     ( set "AUTH=none" & set "URL=%BASE%/video/listByAuthorID" & set "BODY={\"author_id\":1}" & set "BASELINE=1 SQL, big payload (103 rows)" & goto :eof )
if /i "%RM%"=="latest"     ( set "AUTH=none" & set "URL=%BASE%/feed/listLatest" & set "BODY={\"limit\":10}" & set "BASELINE=stage7 DONE: ZSET hot path = 2 Redis RTT + 1 SQL (+23.2%% QPS)" & goto :eof )
if /i "%RM%"=="popular"    ( set "AUTH=none" & set "URL=%BASE%/feed/listByPopularity" & set "BODY={\"limit\":10}" & set "BASELINE=stage8 ZSET target" & goto :eof )
if /i "%RM%"=="bytag"      ( set "AUTH=none" & set "URL=%BASE%/feed/listByTag" & set "BODY={\"tag_name\":\"java\",\"limit\":10}" & set "BASELINE=anonymous" & goto :eof )
if /i "%RM%"=="likescount" ( set "AUTH=none" & set "URL=%BASE%/feed/listLikesCount" & set "BODY={\"limit\":10}" & set "BASELINE=anonymous" & goto :eof )
if /i "%RM%"=="search"     ( set "AUTH=none" & set "URL=%BASE%/feed/search" & set "BODY={\"query\":\"Redis\",\"limit\":10}" & set "BASELINE=INVESTIGATE: currently returns 0 hits" & goto :eof )
if /i "%RM%"=="following"  ( set "URL=%BASE%/feed/listByFollowing" & set "BODY={\"limit\":10}" & set "BASELINE=double auth -- target of stage7 step4" & goto :eof )
if /i "%RM%"=="comments"   ( set "AUTH=none" & set "URL=%BASE%/comment/listAll" & set "BODY={\"video_id\":177}" & set "BASELINE=public read ^(177 exists; 164/168 are orphans -> 500^)" & goto :eof )
if /i "%RM%"=="myliked"    ( set "URL=%BASE%/like/listMyLikedVideos" & set "BODY={}" & set "BASELINE=JWT read" & goto :eof )
if /i "%RM%"=="followers"  ( set "URL=%BASE%/social/getAllFollowers" & set "BODY={\"vlogger_id\":0}" & set "BASELINE=JWT read (0 = myself)" & goto :eof )
if /i "%RM%"=="vloggers"   ( set "URL=%BASE%/social/getAllVloggers" & set "BODY={\"follower_id\":0}" & set "BASELINE=JWT read (0 = myself)" & goto :eof )
if /i "%RM%"=="counts"     ( set "URL=%BASE%/social/getCounts" & set "BODY={}" & set "BASELINE=2 COUNTs per request" & goto :eof )
if /i "%RM%"=="profile"    ( set "AUTH=none" & set "URL=%BASE%/account/getProfile" & set "BODY={\"account_id\":1}" & set "BASELINE=4 aggregates across 3 modules" & goto :eof )
echo   [ERROR] unknown mode "%RM%"  ^(see: scripts\bench.bat list^)
set "RESOLVE_ERR=1"
goto :eof

REM ===========================================================================
REM  :run_all  --  preflight, iterate MODES, print summary from the ledger
REM ===========================================================================
:run_all
set "GOPATH="
for /f "delims=" %%i in ('go env GOPATH 2^>nul') do set "GOPATH=%%i"
set "HEY=!GOPATH!\bin\hey.exe"
if not exist "!HEY!" (
  echo [ERROR] hey not found at !HEY!
  echo         install it with:  go install github.com/rakyll/hey@latest
  exit /b 1
)

REM  `redis` is the LAST column on purpose: rows written before this column
REM  existed simply have 12 fields instead of 13, so every earlier token
REM  position (and every findstr / every historical diff) keeps working.
if not exist "!CSV!" echo timestamp,label,mode,route,sql_per_req,sql_total,qps,p50_s,p95_s,p99_s,ok,bad,redis > "!CSV!"

REM  %DATE% is locale-dependent (it prints a Chinese weekday here) and would
REM  corrupt the CSV. Take one ISO timestamp up front instead.
set "TS="
for /f "delims=" %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyy-MM-ddTHH:mm:ss" 2^>nul') do set "TS=%%i"
if "!TS!"=="" set "TS=unknown"

REM  ---- preflight ----
for /f "tokens=1" %%a in ('tasklist /fi "imagename eq hey.exe" 2^>nul ^| findstr /i "hey.exe"') do set "ORPHAN=1"
if defined ORPHAN (
  echo.
  echo   [WARN] orphan hey.exe found -- killing it.
  echo          Leftover hey keeps hammering the server while you measure,
  echo          which makes every later number fiction.
  taskkill /F /IM hey.exe >nul 2>&1
  ping -n 3 127.0.0.1 >nul
)
REM  Count with findstr + a counter, NOT `find /c`: when this script is launched
REM  from git-bash, `find` resolves to the Unix find, which then walks the whole
REM  C: drive. (Explorer double-click would pick Windows find, so it only breaks
REM  in one of the two ways you run this. Avoid the name entirely.)
set /a TW=0
for /f "delims=" %%a in ('netstat -an ^| findstr /c:"TIME_WAIT"') do set /a TW+=1
echo   TIME_WAIT sockets : !TW!
for /f "delims=" %%a in ('powershell -NoProfile -Command "if([int]!TW! -gt 10000){'HIGH'}else{'ok'}" 2^>nul') do set "TWSTAT=%%a"
if "!TWSTAT!"=="HIGH" (
  echo   [WARN] that is high. Windows has ~16384 dynamic ports and TIME_WAIT
  echo          lasts ~120s. Expect [500]s and collapsed QPS on heavier
  echo          endpoints. Wait 2 minutes, or rerun for a clean baseline.
)

echo.
echo ###########################################################################
echo   myfeed load test     n=!N!  c=!C!  label=!LABEL!
echo   hey: !HEY!
echo ###########################################################################

call :sweep
call :print_summary
goto :done

REM ===========================================================================
REM  :print_summary  --  rebuild the table from the ledger rows of THIS label
REM  Reading it back out of the CSV (instead of accumulating in variables)
REM  is what makes the table survive a mid-sweep abort and lets `ab` print one
REM  table covering both groups.
REM ===========================================================================
:print_summary
echo.
echo ################ SUMMARY  (label=!LABEL!  n=!N!  c=!C!) ################
echo.
echo    mode             redis   sql/req        QPS    p50ms   p95ms   p99ms      ok     bad
echo    --------------  ------  --------  ---------  ------  ------  ------  ------  ------
for /f "tokens=3,5,7,8,9,10,11,12,13 delims=," %%a in ('findstr /c:",!LABEL!," "!CSV!" 2^>nul') do (
  set "MM=%%a              "
  set "MM=!MM:~0,15!"
  set "RS=%%i      "
  set "RS=!RS:~0,6!"
  call :to_ms SP %%d
  call :to_ms SQ %%e
  call :to_ms SR %%f
  echo    !MM! !RS!  %%b   %%c  !SP_MS!  !SQ_MS!  !SR_MS!  %%g  %%h
)
echo.
echo    ledger: !CSV!
echo    view  : scripts\bench.bat ledger
echo.
goto :eof

REM ===========================================================================
REM  :wait_ports  --  port-exhaustion gate, called between modes
REM
REM  Windows has ~16384 dynamic ports and closed sockets sit in TIME_WAIT for
REM  ~120s. Without this gate a 16-mode sweep pushes TIME_WAIT past the limit
REM  and the SECOND HALF of the sweep reports garbage (app returns [500]s
REM  because IT cannot dial MySQL, not because the code is slow).
REM
REM  With the DB pool fixed (db.go now sets MaxIdleConns == MaxOpenConns) this
REM  should almost never trip. It stays as a tripwire: if it does trip, the
REM  pool regressed or something else is leaking sockets.
REM ===========================================================================
:wait_ports
set /a WP_TRIES=0
:wait_ports_loop
set /a TW=0
for /f "delims=" %%a in ('netstat -an ^| findstr /c:"TIME_WAIT"') do set /a TW+=1
if !TW! lss 9000 goto :eof
set /a WP_TRIES+=1
if !WP_TRIES! geq 24 (
  echo   [WARN] TIME_WAIT stuck at !TW! after 4 minutes -- continuing, but treat
  echo          the rest of this sweep as suspect. Check the DB pool settings.
  goto :eof
)
echo   ... TIME_WAIT=!TW! ^(port pressure -- waiting 10s^)
ping -n 11 127.0.0.1 >nul
goto :wait_ports_loop

REM ===========================================================================
REM  :restart_server  --  kill + relaunch the app, then wait for /healthz
REM
REM  Restarting is how the `ab` OFF group gets cache==nil: main.go Pings Redis
REM  at boot with a 300ms timeout and sets cache=nil if that fails. So "stop
REM  redis, then restart" is the only way to measure the true no-cache system.
REM ===========================================================================
:restart_server
taskkill /F /IM myfeed.exe >nul 2>&1
ping -n 3 127.0.0.1 >nul
if not exist "!EXE!" (
  echo   [ERROR] server binary not found at !EXE!
  echo           build it first:  go build -o .run\myfeed.exe ./cmd
  exit /b 1
)
REM  Start-Process rather than `start /B`: a /B child shares this console, so it
REM  dies when the script exits -- the user would be left with no server and no
REM  explanation. Start-Process gives the server its own detached lifetime.
REM  -WorkingDirectory matters: configs\config.yaml is a RELATIVE path, so the
REM  server must start from the project root or it cannot read its config.
REM  (stdout and stderr need separate files; redirecting both to one is rejected.)
powershell -NoProfile -Command "Start-Process -FilePath '!EXE!' -WorkingDirectory '!ROOT!' -WindowStyle Hidden -RedirectStandardOutput '!ROOT!\.run\server.out' -RedirectStandardError '!ROOT!\.run\server.log'" >nul 2>&1

set /a RS_TRIES=0
:restart_server_wait
set /a RS_TRIES+=1
REM  Poll with `curl -f` and the exit code, NOT `-w "%{http_code}"`: inside a
REM  for /f command string cmd would read the bare `%` as a variable reference.
REM  -f makes curl exit non-zero on HTTP >= 400, so this also catches a server
REM  that is listening but returning 500s (e.g. DB unreachable).
curl -s -f -o nul %BASE%/healthz >nul 2>&1
if !errorlevel! equ 0 (
  echo   [server] up after !RS_TRIES! tries  redis=!REDISSTATE!
  goto :eof
)
if !RS_TRIES! geq 40 (
  echo   [ERROR] server did not come up after 40s. Tail of .run\server.log:
  powershell -NoProfile -Command "Get-Content '!ROOT!\.run\server.log' -Tail 12" 2>nul
  exit /b 1
)
ping -n 2 127.0.0.1 >nul
goto :restart_server_wait

REM ===========================================================================
REM  :flush_cache  --  cold cache, so each group starts from the same state
REM  Without this, group 1 warms the cache and group 2's numbers depend on
REM  which group ran first -- the classic way to fake an A/B result.
REM ===========================================================================
:flush_cache
if /i "!REDISSTATE!"=="on" (
  !RCLI! FLUSHDB >nul 2>&1
  echo   [redis] flushed -- cold cache
) else (
  echo   [redis] skipped -- container is down
)
goto :eof

:start_redis
docker start !REDIS_CTR! >nul 2>&1
set /a SR_TRIES=0
:start_redis_wait
set /a SR_TRIES+=1
for /f "delims=" %%a in ('!RCLI! PING 2^>nul') do set "PONG=%%a"
if "!PONG!"=="PONG" (
  echo   [redis] container up
  goto :eof
)
if !SR_TRIES! geq 20 (
  echo   [ERROR] redis did not answer PING after 20s
  exit /b 1
)
ping -n 2 127.0.0.1 >nul
goto :start_redis_wait

:stop_redis
docker stop !REDIS_CTR! >nul 2>&1
echo   [redis] container stopped
goto :eof

REM ===========================================================================
REM  :run_one  --  one endpoint: SQL before/after + hey + parse + CSV row
REM ===========================================================================
:run_one
call :resolve %1
if defined RESOLVE_ERR goto :eof
set "M=%~1"

set "Q_BEFORE="
for /f "tokens=2" %%a in ('!MYSQL_CTR! "SHOW GLOBAL STATUS LIKE 'Com_stmt_execute'" 2^>nul') do set "Q_BEFORE=%%a"

echo.
echo ---------------------------------------------------------------------------
echo   [%M%]  !METHOD!  !URL!
echo   baseline : !BASELINE!
if not "!Q_BEFORE!"=="" echo   sql before: !Q_BEFORE!
echo ---------------------------------------------------------------------------

if /i "!METHOD!"=="GET" (
  "!HEY!" -n !N! -c !C! "!URL!" > "!LOG!" 2>&1
) else (
  "!HEY!" -n !N! -c !C! -m POST -H "Content-Type: application/json" -H "Authorization: Bearer !TOKEN!" -d "!BODY!" "!URL!" > "!LOG!" 2>&1
)

set "QPS=" & set "P50=" & set "P95=" & set "P99="
for /f "tokens=2" %%a in ('findstr /c:"Requests/sec:" "!LOG!"') do set "QPS=%%a"
REM  latency distribution lines are the only ones containing " in "
set /a DIST_IDX=0
for /f "tokens=1,3" %%a in ('findstr /c:" in " "!LOG!"') do (
  set /a DIST_IDX+=1
  if !DIST_IDX!==3 set "P50=%%b"
  if !DIST_IDX!==6 set "P95=%%b"
  if !DIST_IDX!==7 set "P99=%%b"
)
REM  status-code distribution: lines like "  [200]	8000 responses"
set /a OKN=0
set /a BADN=0
set "BADCODES="
for /f "tokens=1,2" %%a in ('findstr /c:" responses" "!LOG!"') do (
  if "%%a"=="[200]" ( set /a OKN+=%%b ) else ( set /a BADN+=%%b & set "BADCODES=!BADCODES! %%a=%%b" )
)
set /a SEEN=OKN+BADN
if !SEEN! lss !N! set "BADCODES=!BADCODES! [transport]^=!N!-!SEEN!"

set "SQL_TOTAL=" & set "SQL_PER="
if not "!Q_BEFORE!"=="" (
  for /f "tokens=2" %%a in ('!MYSQL_CTR! "SHOW GLOBAL STATUS LIKE 'Com_stmt_execute'" 2^>nul') do set "Q_AFTER=%%a"
  set /a SQL_TOTAL=!Q_AFTER!-!Q_BEFORE!
  set /a SQL_X100=!SQL_TOTAL!*100/!N!
  set /a SQL_INT=!SQL_X100!/100
  set /a SQL_FRAC=!SQL_X100!%%100
  if !SQL_FRAC! lss 10 set "SQL_FRAC=0!SQL_FRAC!"
  set "SQL_PER=!SQL_INT!.!SQL_FRAC!"
  echo   sql after : !Q_AFTER!
  echo   SQL / REQ : !SQL_PER!    ^(!SQL_TOTAL! statements for !N! requests^)
) else (
  set "SQL_PER=n/a"
  echo   SQL       : counter unavailable
)

echo   HTTP      : QPS=!QPS!   ok=!OKN!   bad=!BADN! !BADCODES!
if not "!P50!"=="" (
  call :to_ms P50 "!P50!"
  call :to_ms P95 "!P95!"
  call :to_ms P99 "!P99!"
  echo   LATENCY   : p50=!P50_MS!ms  p95=!P95_MS!ms  p99=!P99_MS!ms
)
if !BADN! gtr 0 (
  echo   [WARN] !BADN! failed requests. A non-200 here means the number in
  echo          this row is NOT a property of your cache -- check TIME_WAIT
  echo          and orphan hey.exe before you draw any conclusion.
)

>>"!CSV!" echo !TS!,!LABEL!,!M!,!URL!,!SQL_PER!,!SQL_TOTAL!,!QPS!,!P50!,!P95!,!P99!,!OKN!,!BADN!,!REDISSTATE!
goto :eof

REM ===========================================================================
REM  :to_ms  --  "0.0048" (secs) -> "<var>_MS=4.8"
REM  drop the dot -> "00048" -> prefix 1 -> 100048 -> -100000 = 48 tenths = 4.8ms
REM  The leading 1 is essential: set /a reads a leading 0 as OCTAL.
REM ===========================================================================
:to_ms
set "RAW=%~2"
set "RAW=!RAW:.=!"
set /a "TENTHS=1!RAW! - 100000"
set /a "MS_INT=TENTHS/10"
set /a "MS_FRAC=TENTHS%%10"
set "%~1_MS=!MS_INT!.!MS_FRAC!"
goto :eof

:list_modes
echo.
echo   MODE          ROUTE                              TOKEN  BASELINE
echo   ------------  ---------------------------------  -----  ---------------------------
echo   health        GET  /healthz                      no     0 SQL, pure HTTP ceiling
echo   auth          POST /like/isLiked                 yes    1 SQL ^<-- JWT cache DONE
echo   video         POST /video/getDetail              yes    0 SQL ^<-- stage7 step2 DONE (4.4x)
echo   author        POST /video/listByAuthorID         yes    1 SQL, big payload
echo   latest        POST /feed/listLatest              yes    1 SQL ^<-- stage7 DONE (+23.2%%)
echo   popular       POST /feed/listByPopularity        yes    stage8 ZSET target
echo   bytag         POST /feed/listByTag               yes    2 SQL
echo   likescount    POST /feed/listLikesCount          yes    1 SQL
echo   search        POST /feed/search                  yes    returns 0 hits -- investigate
echo   following     POST /feed/listByFollowing         yes    0 SQL ^<-- stage7 step4 DONE
echo   comments      POST /comment/listAll              yes    public read, 2 SQL
echo   myliked       POST /like/listMyLikedVideos       yes    JWT read
echo   followers     POST /social/getAllFollowers       yes    JWT read
echo   vloggers      POST /social/getAllVloggers        yes    JWT read
echo   counts        POST /social/getCounts             yes    2 COUNTs
echo   profile       POST /account/getProfile           yes    4 aggregates, 3 modules
echo.
echo   THE `TOKEN` COLUMN IS ABOUT THE HARNESS, NOT THE ROUTE.
echo   Every POST mode sends `Authorization: Bearer ^<token^>` -- there is no
echo   switch that turns it off. So "TOKEN=yes" means only "this run carried a
echo   valid JWT", and every POST row therefore also exercises SoftJWTAuth's
echo   check() (1 Redis GET when the token cache is warm). GET /healthz is the
echo   only genuinely header-less mode.
echo.
echo   Consequence: a mode whose route is PUBLIC still pays the JWT cache read.
echo   To measure a public route anonymously you must bypass this harness
echo   (curl without the header). Making TOKEN=no actually drop the header is
echo   a known improvement, deliberately not done yet: it would change the
echo   workload of every historical POST row in the ledger, so all of them
echo   would have to be re-baselined in the same pass.
echo.
echo   special:  all ^| ab ^| list ^| ledger
echo.
echo   ab  = full sweep twice: cache ON, then cache OFF ^(stop redis + restart
echo         server so cache==nil at boot^). Rows tagged via the `redis` column.
echo         This is the one to run for a before/after cache story.
echo.
goto :done

:show_ledger
if not exist "!CSV!" (
  echo [ERROR] no ledger yet: !CSV!
  goto :done
)
type "!CSV!"
goto :done

:done
echo.
if defined BENCH_NO_PAUSE goto :eof
pause >nul
endlocal
