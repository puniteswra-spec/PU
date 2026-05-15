#!/bin/bash
# Build SystemHelper.exe with version
# Usage: ./build_agent.sh [version]

VERSION="${1:-6.0.9}"

cd "$(dirname "$0")"

# Update version in source (portable sed)
sed -i.bak "s/const Version = \".*\"/const Version = \"$VERSION\"/" main.go
rm -f main.go.bak

# Build
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o "SystemHelper_v${VERSION}.exe" .

echo "Built: SystemHelper_v${VERSION}.exe ($(ls -lh "SystemHelper_v${VERSION}.exe" | awk '{print $5}'))"
