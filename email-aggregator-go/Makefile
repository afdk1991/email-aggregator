# 跨平台发布（包装 build.sh，提供 make 入口）
# 目标：
#   make            # 全平台构建 + 打包到 ./release
#   make windows    # 仅 Windows 全架构
#   make linux      # 仅 Linux 全架构
#   make darwin     # 仅 macOS 全架构
#   make clean      # 清理 release/ 与 webroot/dist
GO ?= go

.PHONY: all windows linux darwin clean

all:
	./build.sh

windows linux darwin:
	./build.sh $@

clean:
	rm -rf release webroot/dist
	$(GO) clean ./...
