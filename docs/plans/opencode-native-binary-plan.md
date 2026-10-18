# Kế hoạch Triển khai Kiến trúc Binary OpenCode Chính thức (OpenCode Native Architecture)

> **Trạng thái:** Bản thảo (Draft - Chưa triển khai)  
> **Mục tiêu:** Chuyển đổi từ mô hình Reverse Proxy giả lập (`opencode2api`) sang sử dụng 100% Binary OpenCode chính thức (`opencode serve`) kết hợp OpenAI-compatible Bridge nhằm giải quyết triệt để lỗi "Detect Abuse Traffic" và đảm bảo tính ổn định lâu dài.

---

## 1. Bối cảnh & Vấn đề cần giải quyết

### 1.1 Hiện trạng của `opencode2api`
- `opencode2api` là một cổng API Gateway viết bằng Golang, hoạt động theo cơ chế **giả lập HTTP Client**: mô phỏng User-Agent, cấu trúc phiên `x-opencode-session`, các schema tools giả lập (`read`, `shell`) và các luồng stream SSE để gửi trực tiếp lên Cloud API của OpenCode (`https://opencode.ai`).
- Điểm mạnh: Nhẹ (RAM ~30MB), có WebUI quản trị trực quan, quản lý và test nhiều Proxy, hỗ trợ failover.

### 1.2 Nguyên nhân gốc rễ lỗi "Detect Abuse Traffic" & HTTP 403
1. **Lệch chuẩn TLS Fingerprint (JA3/JA4):** Go HTTP Client tạo ra ClientHello khác biệt hoàn toàn so với client chính thức của OpenCode (chạy trên nền Bun/BoringSSL). Hệ thống Cloudflare Bot Management của OpenCode nhận diện đây là automated bot traffic.
2. **IP máy chủ bị gắn cờ:** Kết nối đi thẳng từ IP máy chủ (`direct`) thuộc dải IP datacenter/Việt Nam bị xếp vào nhóm nghi vấn.
3. **Tiến trình Probe định kỳ:** Gateway tự động gửi request thăm dò liên tục tới các model miễn phí, kích hoạt ngưỡng phòng thủ chống lạm dụng của máy chủ upstream.

---

## 2. Kiến trúc Đề xuất (Target Architecture)

```text
┌─────────────────────────────────────────────────────────────┐
│                 Client Applications                         │
│  (NextChat, LibreChat, Open WebUI, Claude Code, Cline, etc.)│
└──────────────────────────────┬──────────────────────────────┘
                               │ Chuẩn OpenAI API (/v1/chat/completions)
                               ▼
┌─────────────────────────────────────────────────────────────┐
│              OpenAI-Compatible Bridge Service               │
│               (Port 20160 - Cổng đối ngoại)                 │
│  - Tiếp nhận request chuẩn OpenAI / Anthropic format         │
│  - Chuyển đổi sang REST API của OpenCode Native Server      │
│  - Xử lý xác thực Token & Model Mapping                     │
└──────────────────────────────┬──────────────────────────────┘
                               │ Internal HTTP / SSE (Port 4096)
                               ▼
┌─────────────────────────────────────────────────────────────┐
│             OpenCode Native Daemon (Official CLI)           │
│                   `opencode serve --port 4096`              │
│  - 100% Runtime gốc OpenCode (Bun / Node.js)                │
│  - Đúng chuẩn TLS ClientHello, Session State & Agent Tools  │
│  - Miễn nhiễm hoàn toàn với Bot Detection / Abuse Traffic   │
└──────────────────────────────┬──────────────────────────────┘
                               │ Egress via HTTP_PROXY / HTTPS_PROXY
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                     Egress Proxy Pool                       │
│    (Residential / US Proxy sạch - có hỗ trợ rotating)       │
└──────────────────────────────┬──────────────────────────────┘
                               │ Internet (HTTPS)
                               ▼
┌─────────────────────────────────────────────────────────────┐
│                   OpenCode Cloud API                        │
│                 (https://opencode.ai)                       │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. Đánh giá Môi trường Server Hiện tại (`thanhpk@192.168.1.68`)

Đã kiểm tra thực tế trên server và xác nhận **100% đủ điều kiện**:
- **Hệ điều hành:** Ubuntu 24.04 LTS x86_64, Linux kernel `6.8.0`.
- **Runtime có sẵn:** Node.js `v24.21.0`, npm `11.19.0`.
- **Tài nguyên:** RAM khả dụng ~3.8 GB, dung lượng ổ đĩa trống ~19 GB (đáp ứng thoải mái cho tiến trình `opencode serve` tốn ~150-300MB RAM).
- **Package chính thức:** `opencode-ai` bản mới nhất `1.19.3` có sẵn trên npm.

---

## 4. Kế hoạch Triển khai Chi tiết (Phased Execution)

### Giai đoạn 1: Thiết lập Daemon OpenCode Chính thức
1. **Cài đặt package CLI toàn cục:**
   ```bash
   npm install -g opencode-ai
   ```
2. **Tạo thư mục cấu hình và chạy thử nghiệm:**
   - Thư mục làm việc: `/home/thanhpk/opencode-native/`
   - File cấu hình môi trường `.env`:
     ```env
     HTTP_PROXY=http://1111x62:dd1431@14.241.72.179:20370
     HTTPS_PROXY=http://1111x62:dd1431@14.241.72.179:20370
     OPENCODE_SERVER_PASSWORD=your_secure_password
     ```
3. **Cấu hình Systemd Service quản lý tiến trình nền:**
   - Tạo file service `/etc/systemd/system/opencode-daemon.service`:
     - Chạy lệnh: `opencode serve --hostname 127.0.0.1 --port 4096`
     - Tự động restart khi crash (`Restart=always`, `RestartSec=5s`).
     - Tích hợp nạp biến môi trường Proxy từ file `.env`.

### Giai đoạn 2: Triển khai Cổng Tương thích (OpenAI API Bridge)
1. **Lựa chọn giải pháp Bridge:**
   - Sử dụng giải pháp bridge mã nguồn mở chuyên dụng cho OpenCode CLI (ví dụ: `opengate` hoặc `personal-opencode-zen-gateway`).
   - Thiết lập cổng lắng nghe bên ngoài: **`20160`** (không xung đột với cổng `20150/20151` của gateway hiện tại).
2. **Cấu hình Model Mapping & Quản lý Key:**
   - Thiết lập Bearer Token cho các client gọi vào.
   - Định tuyến các model phổ biến:
     - `deepseek-v4.1-flash` -> `deepseek-v4.1-flash-free`
     - `glm-5.3` -> `glm-5.3-flash-free`
     - `kimi-k3` -> `kimi-k3-free`

### Giai đoạn 3: Kiểm thử Đối chuẩn & Đo lường
1. **Kiểm tra khả năng vượt qua Bot Detection:**
   - Gửi các request test gọi model free từ client bên ngoài thông qua cổng `20160`.
   - Xác nhận response trả về `200 OK`, stream SSE mượt mà, không còn xuất hiện lỗi `FreeTierError` hay `Detect Abuse Traffic`.
2. **Đo lường hiệu năng và độ ổn định:**
   - Tốc độ phản hồi thời gian đầu (TTFT - Time To First Token).
   - Kiểm tra mức tiêu thụ RAM và CPU của tiến trình daemon sau 24h chạy liên tục.

### Giai đoạn 4: Tự động hóa Cập nhật (Auto-update Pipeline)
1. **Script cập nhật 1 chạm (`/home/thanhpk/update-opencode-native.sh`):**
   ```bash
   #!/usr/bin/env bash
   set -euo pipefail
   echo "[$(date)] Kiểm tra cập nhật OpenCode CLI..."
   npm update -g opencode-ai
   systemctl restart opencode-daemon
   systemctl restart opencode-bridge
   echo "[$(date)] Cập nhật và khởi động lại dịch vụ thành công!"
   ```
2. **Cronjob định kỳ:**
   - Thiết lập cronjob chạy tự động vào 03:00 Chủ nhật hàng tuần để kéo bản vá upstream mới nhất mà không cần can thiệp thủ công.

### Giai đoạn 5: Chuyển đổi Chính thức (Cutover) & Thu hồi Hệ thống Cũ
1. **Chuyển đổi Client:**
   - Cập nhật URL API trên các client từ port `20150` sang port `20160`.
2. **Dừng và thu hồi tài nguyên cũ:**
   - Khi hệ thống mới chạy ổn định tuyệt đối sau 3–5 ngày, tiến hành dừng container `opencode2api` cũ để giải phóng RAM và port `20150/20151`.

---

## 5. Kế hoạch Dự phòng & Quản lý Rủi ro (Risk & Rollback)

| Rủi ro tiềm ẩn | Mức độ | Biện pháp giảm thiểu |
|---|---|---|
| **Egress Proxy gặp sự cố** | Trung bình | Cấu hình script tự kiểm tra ping của proxy trong file `.env`, tự động switch sang proxy dự phòng nếu proxy chính die. |
| **Bridge Service chiếm dụng RAM** | Thấp | Cấu hình giới hạn tài nguyên trong systemd (`MemoryMax=512M`). |
| **Xung đột khi chưa nghiệm thu** | Rất thấp | Chạy song song độc lập trên port riêng (`20160`), container cũ trên port `20150` vẫn hoạt động 100% nguyên vẹn. |

**Phương án Rollback:**  
Nếu kiến trúc mới không đạt yêu cầu, chỉ cần tắt service `systemctl stop opencode-daemon opencode-bridge`. Mọi lưu lượng client vẫn trỏ về container `opencode2api` trên port `20150` như hiện tại mà không có bất kỳ thời gian gián đoạn (zero downtime) nào.
