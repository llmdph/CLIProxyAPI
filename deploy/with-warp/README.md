# 217 的 CPA + Warp

这是 217 上正在跑的启动方式：CPA 镜像自己构建，出站走 `socks5://warp-lb:1080`，管理页用仓库里的 `static/management.html`。

账号目录、管理密钥、插件二进制不在 git 里。只克隆仓库能把进程拉起来，但没有旧机器上的账号。

## 新机器

两个仓库放在同一层目录：

```bash
git clone git@github.com:llmdph/warp-lb-rotate-agent.git
git clone git@github.com:llmdph/CLIProxyAPI.git
cd CLIProxyAPI
git checkout fix/reuse-proxy-http-client-pool
sudo WARP_SRC=../warp-lb-rotate-agent bash deploy/with-warp/bootstrap.sh
```

`bootstrap.sh` 会先装 warp 池，再构建 `cli-proxy-api:http-pool`，然后在 `/opt/cli-proxy-api` 启动。217 的端口保持不变：`8317`、`8085`、`1455`、`54545`、`51121`、`11451`。

要连同现网账号一起搬：

```bash
sudo bash deploy/with-warp/pull-runtime.sh root@OLD_HOST
sudo docker compose -f /opt/cli-proxy-api/docker-compose.yml up -d
```

`pull-runtime.sh` 复制 `config.yaml`、`auths/`、`plugins/`、`static/`、`data/`，不复制日志。复制之后现网的 `proxy-url: socks5://warp-lb:1080` 不用改。`warp-rotate-url` 在 217 上是注释掉的，轮换由 warp 机器上的 systemd 自己做。

217 的 nginx 把 `cpa.llmdph.cc.cd` 反代到 `127.0.0.1:8317`。证书不在这个仓库，域名要在新机器上另接。

grok2api 不是 CPA 启动的必要条件。warp 和 CPA 都会挂上外部网络 `grok2api_default`，所以这张网会被先建出来。要同时跑 grok2api，用 `llmdph/grok2api` 的 `personal/console-full-stack`，并让容器加入同名网络。
