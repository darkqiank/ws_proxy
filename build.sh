#!/bin/bash

# 版本信息
VERSION="1.0.0"
BUILD_DIR="build"

# 创建构建目录
mkdir -p ${BUILD_DIR}

# 支持的系统和架构
OSES="linux darwin windows"
ARCHES="amd64 arm64"

# 设置版本号
go_ldflags="-X main.Version=${VERSION}"

echo "=== 开始构建 GoTunnel v${VERSION} ==="

# 构建所有平台
for os in ${OSES}; do
  for arch in ${ARCHES}; do
    # 跳过 Windows ARM64
    if [ "$os" = "windows" ] && [ "$arch" = "arm64" ]; then
      continue
    fi

    echo "构建 $os/$arch..."
    
    # 设置输出目录
    output_dir="${BUILD_DIR}/gotunnel-${os}-${arch}"
    mkdir -p "$output_dir"
    
    # 设置客户端二进制文件名
    client_output="${output_dir}/client"
    if [ "$os" = "windows" ]; then
      client_output="${client_output}.exe"
    fi
    
    # 设置服务端二进制文件名
    server_output="${output_dir}/server"
    if [ "$os" = "windows" ]; then
      server_output="${server_output}.exe"
    fi
    
    # 编译客户端
    GOOS=${os} GOARCH=${arch} go build -ldflags "${go_ldflags}" -o ${client_output} ./client
    if [ $? -ne 0 ]; then
      echo "客户端构建失败: ${os}/${arch}"
      exit 1
    fi
    
    # 编译服务端
    GOOS=${os} GOARCH=${arch} go build -ldflags "${go_ldflags}" -o ${server_output} ./server
    if [ $? -ne 0 ]; then
      echo "服务端构建失败: ${os}/${arch}"
      exit 1
    fi
    
    # 复制配置文件
    cp client/config.json "${output_dir}/client-config.json"
    cp server/config.json "${output_dir}/server-config.json"
    cp README.md "${output_dir}/"
    
    # 打包
    cd ${BUILD_DIR}
    if [ "$os" = "windows" ]; then
      zip -r "gotunnel-${os}-${arch}.zip" "gotunnel-${os}-${arch}"
    else
      tar -czvf "gotunnel-${os}-${arch}.tar.gz" "gotunnel-${os}-${arch}"
    fi
    cd ..
    
    echo "✓ ${os}/${arch} 构建完成"
  done
done

echo "=== 构建完成 ==="
echo "构建文件位于: ${BUILD_DIR} 目录" 