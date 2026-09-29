# 多阶段构建。
#
# 最终镜像用 distroless: 里面没有 shell、没有包管理器、没有 libc 之外的
# 任何东西。这个项目的容器会执行候选人提交的代码(判题沙箱会调用宿主
# 容器运行时, 而不是在自身进程里跑), 因此"进程被攻破之后能做什么"
# 必须被压到最小 —— 一个没有 shell 的镜像本身就消掉了一整类攻击面。
#
# 注意: 判题沙箱需要访问容器运行时。生产部署时应当把面试服务与判题
# 执行器分开(见 deployments/k8s), 而不是给面试服务挂 docker.sock ——
# 挂载 sock 等于把宿主机 root 交给它。

FROM golang:1.22-alpine AS build

WORKDIR /src

# 先只拷贝依赖清单, 让 go mod download 这一层能被缓存。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# CGO_ENABLED=0: 静态链接, 不依赖基础镜像里的 libc 版本。
# -trimpath 与 -s -w: 去掉构建路径与符号表, 减小体积, 也避免把构建机
# 的目录结构写进二进制。
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/interviewd ./cmd/interviewd

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/interviewd /interviewd

# 录制件默认落在 /data/recordings; 生产应当换成对象存储并给它单独的卷。
VOLUME ["/data"]

USER nonroot:nonroot
EXPOSE 8080

# 就绪探针用的是 /readyz(它会真的 ping 存储), 而不是 /healthz:
# "进程活着"和"能接流量"是两件事, 只探测后者会让故障实例继续收流量。
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/interviewd", "-healthcheck", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/interviewd"]
CMD ["-serve", ":8080", "-recording-dir", "/data/recordings"]
