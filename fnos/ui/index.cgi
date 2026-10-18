#!/bin/sh
# fnOS same-origin CGI entry: /cgi/ThirdParty/batchunzip/index.cgi/<path>
# fnOS requires this file to be named exactly index.cgi.
# It hands the request to the app binary, which forwards it to 127.0.0.1:18660.
# This is what makes the app reachable from the fnOS mobile app / FN Connect,
# which only forward fnOS's own port.
for d in "$(dirname "${SCRIPT_FILENAME:-$0}")" "$(dirname "$0")" /var/apps/batchunzip/target/ui; do
    b="$d/../bin/batch-unzip"
    if [ -x "$b" ]; then
        exec "$b" -cgi
    fi
done
printf 'Status: 500 Internal Server Error\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n'
printf '找不到批量解压程序，请在飞牛应用中心重新安装「批量解压」。\n'
