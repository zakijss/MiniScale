#!/bin/sh
# Builds the app for the device (ARMv7) into dist/, stamped with VERSION.
#   VERSION=1.0.0 ./scripts/build.sh
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-dev}
export CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-buildvcs=false
mkdir -p dist
# Tailscale, from its pinned source, with everything MiniScale doesn't
# use compiled out (it saves RAM and space). useexitnode, useroutes and
# taildrop serve the exit node, subnet route and Taildrop options.
TS_TAG=v1.104.6
TS_COMMIT=c54807352ae196919497b3f8fddc3cedf88368b9
TS_FEATURES=netstack,nattraversal,udptransport,outboundproxy,bakedroots,dns,cli,ipnbus,health,ssh,qrcodes,useexitnode,useroutes,taildrop
if [ ! -d build/tailscale ]; then
    git clone -q --depth 1 --branch "$TS_TAG" https://github.com/tailscale/tailscale.git build/tailscale
fi
got=$(git -C build/tailscale rev-parse HEAD)
[ "$got" = "$TS_COMMIT" ] || { echo "tailscale $TS_TAG is $got, expected $TS_COMMIT" >&2; exit 1; }
(
    cd build/tailscale
    export GOWORK=off # Tailscale is its own module
    tags="$(GOOS= GOARCH= go run ./cmd/featuretags --min --add=$TS_FEATURES),ts_include_cli"
    eval "$(GOOS= GOARCH= go run ./cmd/mkversion)"
    go build -tags="$tags" -trimpath \
        -ldflags "-w -s -X tailscale.com/version.longStamp=$VERSION_LONG -X tailscale.com/version.shortStamp=$VERSION_SHORT" \
        -o ../../dist/tailscaled ./cmd/tailscaled
)
ld="-s -w -X github.com/zakijss/minideck/version.stamp=minideck-version=$VERSION"
for c in miniscale miniscale-fwd miniscale-badge miniscale-wg; do
    go build -trimpath -ldflags "$ld" -o "dist/$c" "./cmd/$c"
done
ls -l dist
