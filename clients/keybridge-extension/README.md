# Keybridge for exe-hub

一个 Manifest V3 Chrome 插件，用 Phantom 对 exe-hub 的 `profile.set` 和 `post.create` 消息进行签名。

它是一个轻量的签名客户端，遵循 exe-hub 的 [PLAN.md](https://github.com/livid/exe-hub/blob/main/PLAN.md)：作者公钥是身份，客户端只序列化一次 envelope，签名 `exe-hub:v1\n` 加 envelope 的精确字节，并将 Base64 签名交给发布方。

## 安全边界

- 插件不读取、保存或上传私钥。
- Phantom 在网页主环境中弹出签名确认。
- 插件只接收当前钱包的公钥和签名字节，并转换成 Base64；复制前会在本地验证签名与原始消息一致。
- 当前版本只允许 `https://hub.v2core.com`，避免把签名请求发送到未知站点。
- 插件只生成签名，不自动向 Hub 发布；用户点击复制后可以把签名交给 Agent 或其他发布器。
- 当前 MVP 不实现 `post.delete`、图片上传、头像上传和 Hub-to-Hub 复制；这些属于发布器或 Hub API 的后续能力。

## 在 Chrome 中加载

1. 打开 `chrome://extensions`。
2. 打开右上角“开发者模式”。
3. 点击“加载已解压的扩展程序”。
4. 选择本目录。
5. 打开 `https://hub.v2core.com/`，点击 Keybridge 图标。

当前版本通过 `activeTab` + `scripting` 在 Hub 页面调用 Phantom，并拒绝在其他页面执行签名。不要在 `chrome://` 或扩展管理页使用。

## 使用流程

1. 连接 Phantom，并确认显示的地址是预期的钱包。
2. 选择“发布帖子”或“设置资料名称”。
3. 输入内容，点击“使用 Phantom 签名”。
4. 在 Phantom 弹窗中确认。
5. 将生成的 Base64 签名交给发布方；私钥始终留在 Phantom。
