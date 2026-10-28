# 部署到云服务器(让任何人都能访问)

本目录提供最小可用的生产部署: 面试服务 + MySQL + Redis + Caddy(自动 HTTPS)。
一台 2 核 4G 的云服务器即可起步(阿里云 / 腾讯云 / 华为云 ECS 均可, 系统用 Ubuntu 或 Debian)。

## 一、准备(这部分需要你操作)

1. 买一台云服务器, 记下它的**公网 IP**, 并放行安全组端口 **80 和 443**(SSH 22 建议也放行)。
2. 准备一个**域名**, 在 DNS 里加一条 A 记录指向服务器公网 IP。没有域名也能先跑, 见文末"无域名"。
3. 在服务器上安装 Docker 与 Docker Compose(见下)。

## 二、服务器上装 Docker

```bash
curl -fsSL https://get.docker.com | sh
sudo systemctl enable --now docker
```

验证:

```bash
docker --version
docker compose version
```

## 三、拉代码并启动

```bash
git clone https://github.com/zennanzhoudev-pixel/ai-interview-platform.git
cd ai-interview-platform/deploy
cp .env.example .env
nano .env        # 填好 DOMAIN / 各密码 / APP_SECRET / ADMIN_INVITE_CODE, 可选填 LLM_*
docker compose up -d --build
```

查看状态与日志:

```bash
docker compose ps
docker compose logs -f interviewd
```

## 四、注册管理员账号

1. 浏览器打开 `https://你的域名/`。
2. 用「企业成员注册」入口, 填邀请码(即 `.env` 里的 `ADMIN_INVITE_CODE`)注册管理员账号。
3. 登录后进入招聘工作台; 候选人用自己的账号从候选人入口进入面试。

## 五、无域名(先用 IP 快速验证)

没有域名时, 先绕过 Caddy, 直接把服务端口暴露出来:

```bash
cd ai-interview-platform/deploy
docker compose up -d --build mysql redis interviewd
# 临时把 8080 映射出来(仅测试, 别在生产裸奔):
# 在 compose.yml 的 interviewd 服务下临时加一行 ports: ["8080:8080"]
```

然后访问 `http://服务器IP:8080/`。等域名解析好后再切回 Caddy 走 HTTPS。

## 六、常见问题

- **打不开**: 先确认安全组放行了 80/443; `docker compose logs caddy` 看证书/反代报错。
- **账号重启后丢失**: 说明没连上 MySQL, 检查 `.env` 里 `MYSQL_DSN` 对应的账号密码。
- **大模型没生效**: 报告标注"规则评分"说明 `LLM_*` 没配或配错; 配好后在服务器上 `docker compose exec interviewd /interviewd -selftest` 自检。
- **修改代码后更新**: `git pull` 后 `docker compose up -d --build` 即可。
