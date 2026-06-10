#!/bin/sh
# 测试应用脚本

echo "=== mini-docker Build Test ==="
echo "Application: $APP_NAME"
echo "Version: $VERSION"
echo "Working Directory: $(pwd)"
echo ""
echo "Contents of hello.txt:"
cat /app/hello.txt
echo ""
echo "=== Test Complete ==="
