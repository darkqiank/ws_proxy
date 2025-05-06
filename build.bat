@echo off
setlocal enabledelayedexpansion

:: 版本信息
set VERSION=1.0.0
set BUILD_DIR=build

:: 创建构建目录
if not exist %BUILD_DIR% mkdir %BUILD_DIR%

echo === 开始构建 GoTunnel v%VERSION% ===

:: 构建 Windows amd64
set os=windows
set arch=amd64

echo 构建 %os%/%arch%...

:: 设置输出目录
set output_dir=%BUILD_DIR%\gotunnel-%os%-%arch%
if not exist !output_dir! mkdir !output_dir!

:: 编译客户端
set GOOS=%os%
set GOARCH=%arch%
go build -ldflags "-X main.Version=%VERSION%" -o !output_dir!\client.exe .\client
if %errorlevel% neq 0 (
    echo 客户端构建失败: %os%/%arch%
    exit /b 1
)

:: 编译服务端
go build -ldflags "-X main.Version=%VERSION%" -o !output_dir!\server.exe .\server
if %errorlevel% neq 0 (
    echo 服务端构建失败: %os%/%arch%
    exit /b 1
)

:: 复制配置文件
copy client\config.json !output_dir!\client-config.json
copy server\config.json !output_dir!\server-config.json
copy README.md !output_dir!\

echo ✓ %os%/%arch% 构建完成

:: 打包仅供本地使用
echo 注意：这个脚本没有自动打包功能，请手动压缩 %BUILD_DIR%\gotunnel-%os%-%arch% 目录

echo === 构建完成 ===
echo 构建文件位于: %BUILD_DIR% 目录

endlocal 