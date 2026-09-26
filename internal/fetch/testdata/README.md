# NASA Science APOD 测试样本

于 2026-09-26 从 NASA Science 的公开 `wp-json/wp/v2/image-article` 接口获取。JSON 保留文章链接、标题和 `hds-media-detail-hero` 主内容区域，删除站点导航等外围内容；仅用于离线解析回归测试。

- `science_image.json`：2026-09-25，普通图片、版权和迁移公告。
- `science_rollover.json`：2026-09-24，原图／标注图切换，只有 Credit、无 Copyright。
- `science_historical.json`：2000-01-01，历史文章和补零日期。

各样本的 `link` 字段记录来源。视频、非法内容及多段说明使用测试中构造的最小 HTML，避免依赖线上可用性。

- `nasa_basic.json`：`apod-basic?date=2026-09-11&api_key=DEMO_KEY` 实际返回数组中的匹配记录，删除未使用的 `basic_html`；验证图片 `hdurl` 和 HTML 文本映射。
