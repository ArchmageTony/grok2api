# 出口账号禁言守护

独立逻辑容器: 从质量守护内部 API 增量读取请求审计, 按账号归因 TPS 异常并自动处置。
不监听任何端口, 不修改节点状态, 只通过管理员 API 禁用/恢复账号。

## 策略

- 任意一次降智命中: 立即永久禁用该账号, 本守护**永不自动恢复**; 确认误伤后由管理员在账号管理中手动解禁。
- 不迁移账号, 不清空绑定, 只翻 enabled 开关。
- 已禁用的账号重复命中只记录日志, 不重复调用禁用。

## 命中口径

满足以下任一条件且输出 ≥32 Token 的完成请求计为一次降智命中:

- 面板同口径输出 TPS(含推理 Token) 达到软/硬阈值;
- 推理 Token 为 0 (`missing_thinking`)。

部署策略为质量优先: 宁可误禁(短回复仍不计), 账号库存充足时误伤成本低于放行降智。

## 配置

内部凭据与阈值读共享卷中的 `bootstrap.json`(由主程序生成); 管理员凭据默认读挂载的
`config.yaml` 的 `bootstrapAdmin` 段, 无需额外文件。可调环境变量:

| 变量 | 默认 | 说明 |
|---|---|---|
| `GROK2API_BASE_URL` | `http://grok2api:8000` | 主程序内网地址 |
| `GROK2API_BOOTSTRAP_FILE` | `/var/lib/grok2api-quality-guard/bootstrap.json` | bootstrap 路径 |
| `GROK2API_CONFIG_FILE` | `/run/grok2api/config.yaml` | 主程序配置路径 |
| `GROK2API_ADMIN_USERNAME` | 空 | 可选; 覆盖 config.yaml 中的管理员用户名 |
| `GROK2API_ADMIN_PASSWORD_FILE` | 空 | 可选; 覆盖 config.yaml 中的管理员密码 |
| `ACCOUNT_GUARD_WINDOW_SECONDS` | 86400 | 命中计数滚动窗口, 3600-604800 |
| `ACCOUNT_GUARD_PROVIDER` | `grok_build` | 账号池 |

## 运行

Compose 集成见部署仓库主 `docker-compose.yml` 的 `account-guard` profile。

测试:

```sh
python3 -m unittest -v tools/egress-account-guard/account_guard_test.py
```

## 安全边界

- 不输出账号内容, 凭据, 代理地址; 日志只含账号 ID, 事件与 TPS 数值。
- 状态文件原子写入且权限 0600; 进程排他锁防止重复运行。
- 管理员密码只存在于内存; 建议使用 `*_FILE` 注入并设置宿主权限 0600。
