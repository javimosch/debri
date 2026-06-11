#!/bin/bash
# Build script for debri
# Compiles to a single static binary

set -e

APP_NAME="debri"

echo "Building ${APP_NAME}..."

go build -ldflags="-s -w" -o ${APP_NAME} .

echo "Built: ${APP_NAME}"
ls -lh ${APP_NAME}

echo ""
echo "Usage:"
echo "  ./${APP_NAME} \"say hi\""
echo "  ./${APP_NAME} --model SWE-1.6 --stream \"create a file\""
echo "  ./${APP_NAME} --json \"summarize this\""
echo "  ./${APP_NAME} --file prompt.txt"
