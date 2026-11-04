#!/bin/sh
# Fail container start-up instead of serving without a site: the image's
# template step only logs an error when it cannot render, and nginx would then
# start with no server block.
set -eu
conf=/etc/nginx/conf.d/default.conf
if [ ! -s "$conf" ]; then
    echo "$0: ERROR: $conf was not rendered from /etc/nginx/templates" >&2
    exit 1
fi
if grep -q '\${API_UPSTREAM}\|\${NGINX_LOCAL_RESOLVERS}' "$conf"; then
    echo "$0: ERROR: unrendered template variables in $conf" >&2
    exit 1
fi
nginx -t -q
