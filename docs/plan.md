# Kế hoạch Dự án: OpenCode Process Supervisor & Proxy Adapter Gateway

Chi tiết kế hoạch triển khai kiến trúc Supervisor & Proxy Adapter cho Binary OpenCode chính thức được lưu trữ tại:  
👉 **[Kế hoạch Triển khai Kiến trúc Process Supervisor & Proxy Adapter](plans/opencode-native-binary-plan.md)**

---

### Tóm tắt Kiến trúc Mới:
- **Tên dự án:** OpenCode Process Supervisor & Proxy Adapter Gateway.
- **Trạng thái:** Bản thảo Đã cập nhật (Draft - Chờ phê duyệt triển khai).
- **Mô hình hoạt động:**
  1. **WebUI Quản trị Toàn diện:** Giữ nguyên các tính năng quản lý Proxy (test latency, thêm/sửa/xóa, xoay IP), quản lý API Key cho client, bổ sung tab giám sát trạng thái Daemon (PID, Uptime, Proxy đang dùng).
  2. **Process Supervisor:** Quản lý vòng đời tiến trình con `opencode serve --port 4096`, tiêm động biến môi trường `HTTP_PROXY`.
  3. **Auto-Failover with Hot-Restart:** Khi proxy hiện tại bị lỗi/chết hoặc dính 403, Gateway tự động chọn proxy khỏe tiếp theo, ngắt và khởi động lại daemon với proxy mới rồi retry request liền mạch cho client.
  4. **Bộ lọc Free-Only:** Lọc sạch endpoint `/v1/models` và chặn request, đảm bảo client bên ngoài chỉ thấy và chỉ dùng được các model miễn phí.
  5. **Nâng cấp một chạm:** Hỗ trợ cập nhật binary OpenCode CLI trực tiếp từ WebUI (`npm update -g opencode-ai`).
