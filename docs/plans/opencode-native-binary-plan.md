# Kế hoạch Triển khai: OpenCode Process Supervisor & Proxy Adapter Gateway

> **Trạng thái:** Bản thảo Đã cập nhật (Draft - Chờ phê duyệt triển khai)  
> **Mục tiêu:** Xây dựng cổng Gateway kiêm Bộ giám sát tiến trình (Process Supervisor) cho Binary OpenCode chính thức (`opencode serve`). Kết hợp WebUI quản trị Proxy thông minh, tự động xoay/khởi động lại daemon khi proxy lỗi, quản lý API Key và lọc chỉ cung cấp các model miễn phí cho client bên ngoài.

---

## 1. Vấn đề & Định hướng Kiến trúc

### 1.1 Điểm yếu của các giải pháp hiện tại
1. **Gateway Reverse Proxy thuần túy (`opencode2api` hiện tại):**
   - Phải tự giả lập HTTP request, User-Agent, tool schema và TLS ClientHello bằng Golang. Dễ bị Cloudflare Bot Management của OpenCode phát hiện là automated bot traffic và chặn bằng lỗi `FreeTierError` / `Detect Abuse Traffic`.
2. **Binary OpenCode chính thức đứng độc lập (`opencode serve`):**
   - Mang 100% chữ ký TLS và logic agent chính chủ, hoàn toàn không bị chặn bot.
   - **Tuy nhiên:** Không có WebUI quản trị, không có hệ thống quản lý nhiều proxy (chỉ nhận 1 proxy cố định qua `HTTP_PROXY`, chết proxy là sập dịch vụ), không thể lọc model (client có thể gọi nhầm model trả phí) và không có phân quyền API Key độc lập cho từng client.

### 1.2 Định hướng: Kiến trúc Supervisor & Proxy Adapter Gateway
Xây dựng tầng Gateway trung gian thông minh (kế thừa toàn bộ WebUI và Proxy Manager của dự án hiện tại) đóng 3 vai trò:
- **Process Supervisor:** Tự động điều phối vòng đời tiến trình con `opencode serve`, nạp proxy động vào biến môi trường `HTTP_PROXY`.
- **Auto-Failover Engine:** Khi phát hiện proxy hiện tại bị lỗi/chết hoặc dính 403, tự động chuyển proxy và restart daemon `opencode serve` ngầm để tự phục hồi kết nối.
- **API Gate & Model Filter:** Tiếp nhận chuẩn OpenAI API, xác thực API Key của client, và lọc danh mục model chỉ trả về các model miễn phí (`free_only`).

---

## 2. Sơ đồ Kiến trúc Tổng thể (System Architecture)

```text
┌────────────────────────────────────────────────────────────────────────┐
│                   Client Applications (Bên ngoài)                      │
│       (NextChat, Open WebUI, LibreChat, Cursor, Cline, Claude Code)     │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Chuẩn OpenAI API (/v1/chat/completions)
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│             OPENCODE SUPERVISOR GATEWAY (Cổng Trung Gian)              │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │ 1. WebUI Admin Console:                                          │  │
│  │    - Giám sát tiến trình OpenCode (Status, PID, Port, Proxy dùng)│  │
│  │    - Quản lý danh sách Proxy Egress (Thêm, sửa, xoá, test ping ms│  │
│  │    - Chế độ dùng Proxy (Sticky, Round-robin, Random)             │  │
│  │    - Quản lý API Key độc lập cho từng ứng dụng client            │  │
│  │    - Quản lý Model & Công tắc "Chỉ cho phép Model Miễn Phí"      │  │
│  │    - Nút thao tác nhanh: Restart Daemon, Kiểm tra Update Binary  │  │
│  ├──────────────────────────────────────────────────────────────────┤  │
│  │ 2. API Adapter & Auth Gate:                                      │  │
│  │    - Xác thực Bearer Token từ client                             │  │
│  │    - GET /v1/models: Lọc sạch chỉ trả về các model Free          │  │
│  │    - POST /v1/chat/completions: Nhận request và chuyển tiếp      │  │
│  ├──────────────────────────────────────────────────────────────────┤  │
│  │ 3. Process Supervisor & Auto-Failover Engine:                    │  │
│  │    - Spawn tiến trình con `opencode serve --port 4096`           │  │
│  │    - Tiêm HTTP_PROXY tương ứng với proxy đang được chọn          │  │
│  │    - Bắt lỗi: Nếu proxy timeout / 403 / mất mạng:                │  │
│  │      -> Chọn proxy mới trong pool -> Restart daemon ngầm         │  │
│  │      -> Retry request vừa gửi (Seamless Failover)                │  │
│  └──────────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Gọi nội bộ localhost (127.0.0.1:4096)
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                    OpenCode Official Subprocess                        │
│                   `opencode serve --port 4096`                         │
│  - Chạy bằng runtime chính thức (Node.js / Bun)                     │
│  - Mang 100% TLS Fingerprint gốc, User-Agent & Session thật         │
│  - Miễn nhiễm hoàn toàn với Bot Detection / Detect Abuse Traffic    │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Egress HTTPS qua Proxy được cấp
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                        Active Egress Proxy                             │
│                 (http://user:pass@14.241.72.179:20370...)              │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ Internet (HTTPS)
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                     OpenCode Cloud (opencode.ai)                       │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Các Khối Chức năng Trọng tâm

### 3.1 Bộ Điều khiển Tiến trình & Tự Động Phục Hồi (Process Supervisor)
- **Cơ chế Spawn:** Gateway sử dụng `os/exec` quản lý tiến trình con:
  ```bash
  opencode serve --hostname 127.0.0.1 --port 4096
  ```
  kèm các biến môi trường:
  ```env
  HTTP_PROXY=http://user:pass@proxy-active:port
  HTTPS_PROXY=http://user:pass@proxy-active:port
  NO_PROXY=127.0.0.1,localhost
  ```
- **Hot-Restart Failover (Xử lý khi Proxy chết / lỗi 403):**
  - Khi một request gửi tới daemon bị timeout, lỗi mạng, hoặc trả về HTTP 403 / 429:
    1. Gateway tạm thời giữ kết nối của Client (queue request).
    2. Đánh dấu proxy hiện tại là lỗi trong danh sách.
    3. Chọn proxy hoạt động tiếp theo trong pool.
    4. Gửi tín hiệu ngắt tiến trình daemon cũ (`SIGTERM`) và khởi chạy daemon mới với biến `HTTP_PROXY` của proxy mới (thời gian khởi động lại của `opencode serve` thường chỉ mất ~500ms - 1s).
    5. Gửi lại request của client qua daemon mới. Client nhận được kết quả thành công mà không bị đứt đoạn.

### 3.2 Bộ Lọc Model (Free-Only Enforcement)
- **Mục tiêu:** Ngăn chặn tuyệt đối việc client gọi nhầm hoặc nhìn thấy model tính phí.
- **Cơ chế:**
  - Endpoint `GET /v1/models` lấy danh mục từ daemon, áp dụng bộ lọc regex/metadata: chỉ giữ lại các model có hậu tố `-free` (như `deepseek-v4.1-flash-free`, `glm-5.3-flash-free`, `kimi-k3-free`, `big-pickle`...).
  - Endpoint `POST /v1/chat/completions`: Nếu client truyền model không nằm trong danh mục Free được duyệt, Gateway từ chối ngay lập tức với lỗi `400 Bad Request: Model is restricted to free models only`.

### 3.3 Quản lý API Key cho Client
- Kế thừa toàn bộ hệ thống API Key của `opencode2api`:
  - Tạo, đặt tên và quản lý nhiều API Key cho các client khác nhau (Cursor, NextChat, LibreChat, Open WebUI).
  - Phân tách độc lập: Client bên ngoài chỉ giao tiếp bằng Key do Gateway cấp, không có quyền truy cập vào token gốc của OpenCode.

### 3.4 Quản lý Proxy & Giao diện WebUI
- **WebUI Tabbed hiện tại được nâng cấp thêm Tab "OpenCode Daemon":**
  - Hiển thị trạng thái daemon: Trạng thái (Running/Stopped/Restarting), PID, Port nội bộ, Proxy đang gắn kết, thời gian Uptime.
  - Nút bấm điều khiển: **Restart Daemon**, **Đổi Proxy đang dùng thủ công**.
  - Nút **"Cập nhật OpenCode Binary"**: Khi bấm, Gateway chạy lệnh `npm update -g opencode-ai`, thông báo phiên bản mới và tự restart daemon.
- **Tab Quản lý Proxy:** Giữ nguyên 100% tính năng hiện tại: đo ping ms, thêm hàng loạt, bật/tắt, sửa, xóa, chọn mode Sticky/Round-robin/Random.

---

## 4. Kế hoạch Triển khai Chi tiết

### Giai đoạn 1: Chuẩn bị Môi trường Server (`thanhpk@192.168.1.68`)
1. **Cài đặt OpenCode CLI chính thức:**
   ```bash
   npm install -g opencode-ai
   opencode --version
   ```
2. **Kiểm tra quyền thực thi tiến trình con:**
   - Đảm bảo user chạy service có quyền spawn tiến trình, ghi thư mục log và bind port `4096`.

### Giai đoạn 2: Phát triển Tầng Supervisor & Adapter (Golang)
1. **Kế thừa mã nguồn hiện tại:**
   - Tận dụng toàn bộ core WebUI, I18N, Auth CSRF/Session, Admin API và Proxy Pool Manager.
2. **Thay thế Module Upstream:**
   - Xây dựng gói `internal/supervisor`: quản lý tiến trình con `opencode serve`, pipe stdin/stdout/stderr, cơ chế spawn với environment proxy động.
   - Xây dựng gói `internal/adapter`: chuyển đổi request từ chuẩn OpenAI sang API của `opencode serve` nội bộ (hỗ trợ stream SSE mượt mà).
   - Tích hợp logic Auto-Failover: bắt mã lỗi kết nối -> chọn proxy mới -> restart daemon -> retry request.
3. **Bổ sung API & UI quản trị Daemon:**
   - Endpoint `GET /api/daemon/status`: Trả về trạng thái tiến trình và proxy hiện tại.
   - Endpoint `POST /api/daemon/restart`: Cho phép restart daemon với proxy mới.
   - Endpoint `POST /api/daemon/upgrade`: Chạy lệnh `npm update -g opencode-ai`.

### Giai đoạn 3: Kiểm thử Chức năng & Tự Phục Hồi
1. **Kiểm tra vượt rào Bot Detection:**
   - Gọi request model free qua Gateway, xác nhận không còn dính lỗi `Detect Abuse Traffic` hay `FreeTierError` do dùng binary chính chủ.
2. **Kiểm tra kịch bản Failover (Giả lập Proxy chết):**
   - Chủ động ngắt kết nối của proxy đang active.
   - Xác nhận: Gateway tự phát hiện lỗi, chọn proxy kế tiếp, khởi động lại daemon với proxy mới và hoàn tất request thành công cho client.
3. **Kiểm tra bộ lọc Free-Only:**
   - Thử gọi model không phải free và xác nhận Gateway chặn lại đúng quy định.

### Giai đoạn 4: Đóng gói & Triển khai Bền vững
1. **Cấu hình Systemd Service trên Server:**
   - Đóng gói Gateway thành một service duy nhất quản lý toàn bộ (`/etc/systemd/system/opencode-gateway.service`).
   - Gateway tự động khởi chạy và trông coi tiến trình `opencode serve` con.
2. **Chiến lược Zero-Downtime:**
   - Chạy trên port mới (ví dụ `20160`) để kiểm tra song song với hệ thống cũ.
   - Khi nghiệm thu hoàn tất, chuyển cổng chính hoặc trỏ client sang mà không gây gián đoạn dịch vụ.

---

## 5. Đánh giá Tính Khả thi & Lợi ích Thu được

| Tiêu chí | Mô hình Cũ (`opencode2api`) | Mô hình Mới (Supervisor + Native Binary) |
|---|---|---|
| **Chống Bot / Abuse Traffic** | Phải vá code liên tục mỗi khi upstream siết luật | **Miễn nhiễm 100%** (Dùng đúng runtime Bun/Node của OpenCode) |
| **Giao diện Web Quản trị** | Có sẵn | **Giữ nguyên và bổ sung thêm tab quản lý Daemon** |
| **Quản lý & Xoay Proxy** | Độc lập trong Gateway | **Tự động đổi proxy và restart daemon khi có lỗi** |
| **Lọc Model Miễn Phí** | Cơ bản | **Kiểm soát chặt chẽ ở cả danh mục và tầng thực thi** |
| **Khả năng nâng cấp** | Phải sửa mã nguồn Go | **Chỉ cần 1 nút bấm hoặc 1 lệnh `npm update`** |
