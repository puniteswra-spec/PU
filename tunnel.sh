#!/bin/bash
PORT=${1:-8181}

if ! command -v bore &>/dev/null; then
    echo "bore not found. Installing..."
    if command -v brew &>/dev/null; then
        brew install bore-cli
    elif command -v cargo &>/dev/null; then
        cargo install bore-cli
    else
        echo "Install bore manually: https://github.com/ekzhang/bore"
        echo "Or: curl -L https://github.com/ekzhang/bore/releases/latest/download/bore-x86_64-unknown-linux-musl.tar.gz | tar xz && sudo mv bore /usr/local/bin/"
        exit 1
    fi
fi

echo "Starting bore tunnel on port $PORT..."
bore local "$PORT" --to bore.pub
