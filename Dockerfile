# syntax=docker/dockerfile:1
ARG VERSION=dev
FROM node:24.18.0-bookworm-slim AS web-build

WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-bookworm AS build

ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /src/internal/control/web/dist ./internal/control/web/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X simple_cdn/internal/version.Version=${VERSION}" -o /out/cdn-control ./cmd/control \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X simple_cdn/internal/version.Version=${VERSION}" -o /out/cdn-edge-agent-linux-amd64 ./cmd/edge-agent

FROM debian:12-slim AS nginx-build

ARG NDK_VERSION=0.3.4
ARG NDK_SHA256=14a28063294f645d457b1eb10e3c23bbba44398f1c5f021421b58b6f8ab31662
ARG LUA_NGINX_VERSION=0.10.29
ARG LUA_NGINX_SHA256=ca2c2122b909529bf9d1a89e9a5763835a2bd2629def8cb279c550f638f0a78f
ARG LUA_RESTY_CORE_VERSION=0.1.32
ARG LUA_RESTY_CORE_SHA256=da0d3f052ac2d0d181cd560e5bbf04a571636be41882a84af9e557cbe88a5104
ARG LUA_RESTY_LRUCACHE_VERSION=0.15
ARG LUA_RESTY_LRUCACHE_SHA256=8cf1a22e0d5b8f35cb0b2e14c58fcb3aa505a8fb6e956817f0cdb1f06593f072
ARG OPENRESTY_LUAJIT_VERSION=2.1-20260724
ARG OPENRESTY_LUAJIT_SHA256=f5b09359b2939ccc769949acf42e0ad2721fa9bb7678c34789059cb44977fc8a
ARG NGINX_SHA256=4261dc90e9e47c1c4041276e9aaa3d48ebe2e664f728e14fa95ae6c67d57a08b

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl libpcre2-dev libssl-dev \
        zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY deploy/nginx/VERSION /build/NGINX_VERSION

RUN nginx_version=$(tr -d '[:space:]' </build/NGINX_VERSION) \
    && curl --fail --location --silent --show-error \
        "https://nginx.org/download/nginx-${nginx_version}.tar.gz" --output nginx.tar.gz \
    && curl --fail --location --silent --show-error \
        "https://github.com/vision5/ngx_devel_kit/archive/refs/tags/v${NDK_VERSION}.tar.gz" --output ndk.tar.gz \
    && curl --fail --location --silent --show-error \
        "https://github.com/openresty/lua-nginx-module/archive/refs/tags/v${LUA_NGINX_VERSION}.tar.gz" --output lua-nginx.tar.gz \
    && curl --fail --location --silent --show-error \
        "https://github.com/openresty/lua-resty-core/archive/refs/tags/v${LUA_RESTY_CORE_VERSION}.tar.gz" --output lua-resty-core.tar.gz \
    && curl --fail --location --silent --show-error \
        "https://github.com/openresty/lua-resty-lrucache/archive/refs/tags/v${LUA_RESTY_LRUCACHE_VERSION}.tar.gz" --output lua-resty-lrucache.tar.gz \
    && curl --fail --location --silent --show-error \
        "https://github.com/openresty/luajit2/archive/refs/tags/v${OPENRESTY_LUAJIT_VERSION}.tar.gz" --output openresty-luajit.tar.gz \
    && printf '%s  %s\n' \
        "$NGINX_SHA256" nginx.tar.gz \
        "$NDK_SHA256" ndk.tar.gz \
        "$LUA_NGINX_SHA256" lua-nginx.tar.gz \
        "$LUA_RESTY_CORE_SHA256" lua-resty-core.tar.gz \
        "$LUA_RESTY_LRUCACHE_SHA256" lua-resty-lrucache.tar.gz \
        "$OPENRESTY_LUAJIT_SHA256" openresty-luajit.tar.gz | sha256sum --check --strict \
    && tar -xzf nginx.tar.gz \
    && tar -xzf ndk.tar.gz \
    && tar -xzf lua-nginx.tar.gz \
    && tar -xzf lua-resty-core.tar.gz \
    && tar -xzf lua-resty-lrucache.tar.gz \
    && tar -xzf openresty-luajit.tar.gz \
    && make -C "luajit2-${OPENRESTY_LUAJIT_VERSION}" -j1 \
    && make -C "luajit2-${OPENRESTY_LUAJIT_VERSION}" install PREFIX=/opt/openresty-luajit

ENV LUAJIT_LIB=/opt/openresty-luajit/lib
ENV LUAJIT_INC=/opt/openresty-luajit/include/luajit-2.1

RUN nginx_version=$(tr -d '[:space:]' </build/NGINX_VERSION) \
    && cd "nginx-${nginx_version}" \
    && ./configure \
        --build="simple_cdn nginx ${nginx_version}" \
        --prefix=/opt/cdn-edge/nginx \
        --sbin-path=/opt/cdn-edge/nginx/sbin/nginx \
        --modules-path=/opt/cdn-edge/nginx/modules \
        --conf-path=/opt/cdn-edge/nginx/conf/nginx.conf \
        --http-log-path=/opt/cdn-edge/logs/access.json \
        --error-log-path=/opt/cdn-edge/logs/nginx-error.log \
        --lock-path=/opt/cdn-edge/nginx/run/nginx.lock \
        --pid-path=/opt/cdn-edge/nginx/run/nginx.pid \
        --http-client-body-temp-path=/opt/cdn-edge/nginx/tmp/body \
        --http-fastcgi-temp-path=/opt/cdn-edge/nginx/tmp/fastcgi \
        --http-proxy-temp-path=/opt/cdn-edge/nginx/tmp/proxy \
        --http-scgi-temp-path=/opt/cdn-edge/nginx/tmp/scgi \
        --http-uwsgi-temp-path=/opt/cdn-edge/nginx/tmp/uwsgi \
        --user=www-data --group=www-data \
        --with-cc-opt='-O2 -fstack-protector-strong -fstack-clash-protection -Wformat -Werror=format-security -fPIC -D_FORTIFY_SOURCE=2' \
        --with-ld-opt='-Wl,-z,relro -Wl,-z,now -Wl,-rpath,/opt/cdn-edge/nginx/lib -fPIC' \
        --with-compat --with-pcre-jit --with-threads \
        --with-http_ssl_module --with-http_stub_status_module --with-http_realip_module \
        --with-http_auth_request_module --with-http_v2_module --with-http_v3_module \
        --with-http_slice_module --with-http_gunzip_module --with-http_gzip_static_module \
        --with-stream --with-stream_ssl_module --with-stream_ssl_preread_module \
        --with-stream_realip_module \
        --add-module="/build/ngx_devel_kit-${NDK_VERSION}" \
        --add-module="/build/lua-nginx-module-${LUA_NGINX_VERSION}" \
    && make -j1 \
    && strip objs/nginx

COPY deploy/nginx/nginx.conf /bundle/nginx/conf/nginx.conf
RUN nginx_version=$(tr -d '[:space:]' </build/NGINX_VERSION) \
    && install -D -m 0755 "/build/nginx-${nginx_version}/objs/nginx" /bundle/nginx/sbin/nginx \
    && install -D -m 0644 "/build/nginx-${nginx_version}/conf/mime.types" /bundle/nginx/conf/mime.types \
    && install -D -m 0644 "/build/nginx-${nginx_version}/LICENSE" /bundle/nginx/licenses/nginx.txt \
    && install -D -m 0644 "/build/ngx_devel_kit-${NDK_VERSION}/LICENSE" /bundle/nginx/licenses/ngx_devel_kit.txt \
    && install -D -m 0644 "/build/luajit2-${OPENRESTY_LUAJIT_VERSION}/COPYRIGHT" /bundle/nginx/licenses/openresty-luajit.txt \
    && sed -n '/^Copyright and License$/,/^\[Back to TOC\]/p' \
        "/build/lua-nginx-module-${LUA_NGINX_VERSION}/README.markdown" \
        >/bundle/nginx/licenses/lua-nginx-module.txt \
    && sed -n '/^Copyright and License$/,/^\[Back to TOC\]/p' \
        "/build/lua-resty-core-${LUA_RESTY_CORE_VERSION}/README.markdown" \
        >/bundle/nginx/licenses/lua-resty-core.txt \
    && sed -n '/^Copyright and License$/,/^\[Back to TOC\]/p' \
        "/build/lua-resty-lrucache-${LUA_RESTY_LRUCACHE_VERSION}/README.markdown" \
        >/bundle/nginx/licenses/lua-resty-lrucache.txt \
    && test -s /bundle/nginx/licenses/nginx.txt \
    && test -s /bundle/nginx/licenses/ngx_devel_kit.txt \
    && test -s /bundle/nginx/licenses/openresty-luajit.txt \
    && test -s /bundle/nginx/licenses/lua-nginx-module.txt \
    && test -s /bundle/nginx/licenses/lua-resty-core.txt \
    && test -s /bundle/nginx/licenses/lua-resty-lrucache.txt \
    && make -C "lua-resty-core-${LUA_RESTY_CORE_VERSION}" install \
        PREFIX=/bundle/nginx LUA_LIB_DIR=/bundle/nginx/lib/lua/5.1 \
    && make -C "lua-resty-lrucache-${LUA_RESTY_LRUCACHE_VERSION}" install \
        PREFIX=/bundle/nginx LUA_LIB_DIR=/bundle/nginx/lib/lua/5.1 \
    && install -D -m 0755 "$(readlink -f /opt/openresty-luajit/lib/libluajit-5.1.so.2)" \
        /bundle/nginx/lib/libluajit-5.1.so.2 \
    && printf '%s\n' "$nginx_version" >/bundle/nginx/VERSION \
    && printf '%s\n' \
        '{' \
        "  \"nginx_version\": \"${nginx_version}\"," \
        "  \"ndk_version\": \"${NDK_VERSION}\"," \
        "  \"lua_nginx_version\": \"${LUA_NGINX_VERSION}\"," \
        "  \"lua_resty_core_version\": \"${LUA_RESTY_CORE_VERSION}\"," \
        "  \"lua_resty_lrucache_version\": \"${LUA_RESTY_LRUCACHE_VERSION}\"," \
        "  \"openresty_luajit_version\": \"${OPENRESTY_LUAJIT_VERSION}\"," \
        '  "architecture": "amd64"' \
        '}' >/bundle/nginx/BUILD.json \
    && install -d -m 0755 /opt/cdn-edge \
    && cp -a /bundle/nginx /opt/cdn-edge/nginx \
    && install -d -m 0755 /opt/cdn-edge/logs /opt/cdn-edge/config/nginx /opt/cdn-edge/nginx/run \
    && install -d -o www-data -g www-data -m 0700 \
        /opt/cdn-edge/nginx/tmp/body /opt/cdn-edge/nginx/tmp/fastcgi \
        /opt/cdn-edge/nginx/tmp/proxy /opt/cdn-edge/nginx/tmp/scgi /opt/cdn-edge/nginx/tmp/uwsgi \
    && printf '%s\n' 'worker_processes 1;' 'worker_rlimit_nofile 1024;' >/opt/cdn-edge/config/nginx/cdn-platform-main.conf \
    && printf '%s\n' 'worker_connections 128;' >/opt/cdn-edge/config/nginx/cdn-platform-events.conf \
    && : >/opt/cdn-edge/config/nginx/cdn-platform-stream.conf \
    && printf '%s\n' \
        'server {' \
        '    listen 127.0.0.1:18080;' \
        '    location = /__build_smoke {' \
        '        content_by_lua_block {' \
        '            local lrucache = require "resty.lrucache"' \
        '            ngx.say(type(lrucache))' \
        '        }' \
        '    }' \
        '}' >/opt/cdn-edge/config/nginx/cdn-platform.conf \
    && /opt/cdn-edge/nginx/sbin/nginx -t \
    && /opt/cdn-edge/nginx/sbin/nginx \
    && test "$(curl --fail --silent --show-error http://127.0.0.1:18080/__build_smoke)" = table \
    && /opt/cdn-edge/nginx/sbin/nginx -s quit \
    && mkdir -p /out \
    && tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
        -C /bundle -cf - nginx | gzip -n -9 >/out/cdn-nginx-linux-amd64.tar.gz

FROM scratch AS nginx-artifact
COPY --from=nginx-build /out/cdn-nginx-linux-amd64.tar.gz /

FROM debian:12-slim

ARG VERSION
LABEL org.opencontainers.image.title="simple_cdn" \
      org.opencontainers.image.version="${VERSION}"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates certbot curl python3-certbot-dns-cloudflare restic sqlite3 tzdata util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 cdn-platform \
    && useradd --uid 10001 --gid 10001 --home-dir /var/lib/cdn-platform --shell /usr/sbin/nologin cdn-platform

COPY --from=build /out/cdn-control /usr/local/bin/cdn-control
COPY --from=build /out/cdn-edge-agent-linux-amd64 /usr/local/lib/cdn-platform/cdn-edge-agent-linux-amd64
COPY --from=nginx-build /out/cdn-nginx-linux-amd64.tar.gz /usr/local/lib/cdn-platform/cdn-nginx-linux-amd64.tar.gz
COPY scripts/compose-*.sh /usr/local/lib/cdn-platform/
RUN chmod 0755 /usr/local/lib/cdn-platform/compose-*.sh

USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cdn-control"]
