# komga-lite

用 Go 实现的轻量漫画服务器,兼容 Komga REST API(`/api/v1`),可直接用 **Koharia**(Android)、Mihon/Tachiyomi Komga 扩展等客户端连接。网页端由 `html/template` 服务端渲染,整个程序编译为单个 exe。

## 快速开始

```bash
go build -o komga-lite.exe ./cmd/server
.\komga-lite.exe -config config.yaml
```

首次启动会自动创建默认管理员 `admin@komga.local / admin123`,请尽快更换。
然后用管理员账号登录网页,在「管理」页添加书库(见下),无需改配置文件。

目录组织(与 Komga 一致):

```
书库根目录/          <- 管理页里选择的书库路径
├── 系列名 A/
│   ├── 001.zip      <- 一本册(.zip / .cbz 均可,按内容识别)
│   └── 002.cbz
├── 系列名 B/
│   └── 第3卷.zip
└── 单本.zip          <- 根目录散装书也支持,合并入以根目录名命名的系列
```

启动时自动扫描,也可在网页库页面点「扫描此库」,或调接口
`POST /api/v1/libraries/{id}/scan`(需管理员)。

## 用 Koharia 连接

Koharia 中新建服务器,填写:

- **服务器地址**:`http://<本机IP>:8090`
- **邮箱 / 密码**:即上面的用户(认证走 HTTP Basic,与 Komga 相同)

其他兼容 Komga 的 App(Mihon 的 Komga 扩展等)填法一样。

## 认证方式

| 场景 | 方式 |
|---|---|
| 客户端 App | HTTP Basic(`email:password`)或请求头 `X-API-Key` |
| 网页 | 登录后的会话 Cookie |

```bash
# 生成 API Key(给不支持 Basic 的工具用)
.\komga-lite.exe apikey admin@komga.local

# 添加用户(默认普通用户,加 ADMIN 参数为管理员)
.\komga-lite.exe adduser reader@example.com 密码
.\komga-lite.exe adduser boss@example.com 密码 ADMIN
```

## 已实现的 Komga 端点

| 端点 | 说明 |
|---|---|
| `GET /api/v1/libraries`、`GET /api/v1/libraries/{id}` | 库 |
| `POST /api/v1/libraries/{id}/scan` | 触发扫描(202,管理员) |
| `GET /api/v1/series` | 分页列表,支持 `library_id`、`search`、`read_status`、`page/size/sort` |
| `GET /api/v1/series/{id}`、`/books`、`/thumbnail` | 系列详情 / 书列表 / 封面 |
| `GET /api/v1/books` | 全量书列表(Koharia 全库同步用),支持 `library_id`、`series_id`、`search`、`read_status`、`unpaged`、`deleted=false` |
| `GET /api/v1/books/{id}` | 书详情(`media.pagesCount` 等) |
| `GET /api/v1/books/{id}/page/{n}` | 第 n 页图片(**页码从 1 起**) |
| `GET /api/v1/books/{id}/pages` | 页元信息(尺寸/MIME/大小) |
| `GET /api/v1/books/{id}/thumbnail`、`/file` | 封面 / 原文件下载 |
| `GET/PATCH /api/v1/books/{id}/read-progress` | 阅读进度(与网页阅读器双向同步) |
| `GET /api/v1/collections`、`/readlists`、`/tags`、`/genres` | 空集合占位(客户端兼容) |
| `GET /api/v1/client-settings/user/list` | 空设置映射 `{}` |
| `GET /api/v1/sse/v1/events` | SSE 心跳占位(官方 2.x 此路径亦为 404,客户端视为可选) |
| `GET /api/v1/users/me` | 当前用户 |

分页遵循 Spring 风格:`page` 从 0 起,响应为 `{content, totalElements, totalPages, ...}`;
排序参数如 `sort=name,asc`。DTO 字段集对齐 **Komga 2.x**(以官方演示站真实响应为基准):
`mediaType` 为 MIME(`application/zip`)、`metadata.number` 为字符串、含 `fileLastModified` /
`fileHash` / `seriesTitle` / `oneshot` / `booksMetadata` 等新字段;阅读进度对象含
`created` / `readDate` / `deviceId`。`media.status`:READY / ERROR / UNSUPPORTED。

## 格式支持与容错

- **zip / cbz**:按内容识别(后缀不限),页序自然排序(`page_2` 在 `page_10` 前),支持子目录内图片
- **中文文件名**:自动修复 GBK 编码的 zip 条目名
- **图片**:jpg / png / gif / webp / bmp
- **坏文件**:损坏或加密 zip 标记 ERROR、非压缩包标记 UNSUPPORTED,不影响整库扫描
- **变更检测**:按文件大小 + 修改时间增量重扫

## 配置示例(config.yaml)

```yaml
port: 8090                # 监听端口,默认 8080
database: data/komga-lite.db
# logBodies: false        # 关闭请求/响应体日志
# scanWorkers: 4          # 手动指定扫描并发(默认按机器性能自动判定)
```

## 书库管理(主页)

管理员登录后,主页书库网格即可完成全部管理,没有独立的后台页面:

- **添加**:网格末尾的「＋ 添加书库」卡片 → 弹窗里点「浏览…」从盘符/根目录逐级选择
  服务器上的目录 → 填名称(留空用目录名)→「添加并扫描」自动开始扫描
- **编辑 / 删除**:每张书库卡片右上角的 ⋮ 菜单 →「编辑」可改名、换路径(路径变化自动重扫),
  「删除」级联移除其系列、书籍与阅读进度(磁盘文件不受影响)

普通用户只看到书库列表,不显示管理入口。书库保存在数据库中,重启后保留;
config.yaml 只包含端口、数据库路径等基础设置。

## 扫描性能与封面策略

- **启动时自动探测性能**:CPU 核数 + 150ms 单核 SHA-256 基准(代表扫描负载)+ `/proc/meminfo`
  可用内存,据此决定扫描并发 / 库并发 / 预热并发 / SQLite 连接池。慢 CPU(armv7 等)自动减半,
  小内存(<1GB)自动收紧,启动日志会输出一行决策依据,例如:
  `[perf] CPU=4核 基准=120MB/s 内存可用=812MB → 扫描并发=2 库并发=1 预热=1`
  手动覆盖:`config.yaml` 里写 `scanWorkers: 4`
- **扫描只建索引**:读取 zip 目录结构(页数/尺寸/哈希),不解压像素,大库秒级完成
- **并发**:多个书库同时扫描,库内 worker 池并行分析,批量事务写库
- **懒封面**:封面在第一次被请求时才生成并缓存;扫描完成后后台低优先级预热补齐
- **变更检测**:按文件大小 + 修改时间增量重扫,变化的书自动清理过期生成封面(用户封面保留)

## 封面管理(与 Koharia 兼容)

支持 Komga 2.x 的多封面契约(`GENERATED` 自动生成 / `USER_UPLOADED` 用户上传,选中制):

- 网页阅读器中管理员可点「设为封面」把当前页设为该书封面
- `GET /api/v1/books/{id}/thumbnails` 列出封面;`POST` 上传(multipart 字段 `file`,原样保存);
  `DELETE .../thumbnails/{id}-USER` 删除自定义封面;`PUT .../thumbnails/{id}-GENERATED/selected` 恢复默认
- 系列级同构:`/api/v1/series/{id}/thumbnails` 系列(无自定义时取首册封面)

## 调试日志

服务会输出每个请求的 `方法 路径 状态 耗时`,以及文本类请求体(默认开启;
响应体不记录,二进制请求体跳过,登录密码自动打码)。排查客户端(如 Koharia)
连接问题时,直接看控制台或重定向的日志文件即可。想关掉在 config.yaml 加:

```yaml
logBodies: false
```

## 路线图(未完成)

- [ ] CBR(rar)支持
- [ ] PDF / EPUB 支持(整本下载)
- [ ] OPDS 1.2 / v2 订阅源
- [ ] 元数据(ComicInfo.xml)、系列封面自定义
- [ ] 网页阅读器增强(双页、连续滚动、 RTL)
