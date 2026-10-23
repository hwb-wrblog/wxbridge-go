export class WxBridge {
  constructor(baseURL, token) {
    this.baseURL = baseURL.replace(/\/$/, "");
    this.token = token;
  }

  async request(path, { method = "GET", body, idempotencyKey, raw = false } = {}) {
    const headers = { Authorization: `Bearer ${this.token}` };
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    if (body !== undefined) headers["Content-Type"] = raw ? "application/octet-stream" : "application/json";
    const response = await fetch(this.baseURL + path, {
      method,
      headers,
      body: body === undefined ? undefined : raw ? body : JSON.stringify(body),
      signal: AbortSignal.timeout(180000),
    });
    const result = await response.json();
    if (!response.ok) {
      const error = new Error(result.error || `HTTP ${response.status}`);
      error.status = response.status;
      error.result = result;
      throw error;
    }
    return result;
  }

  status() { return this.request("/v1/status"); }
  qr() { return this.request("/v1/login/qr", { method: "POST" }); }
  checkLogin() { return this.request("/v1/login/check", { method: "POST" }); }
  upload(bytes) { return this.request("/v1/uploads", { method: "POST", body: bytes, raw: true }); }
  messages(cursor = 0, peer = "") {
    return this.request(`/v1/messages?cursor=${cursor}&peer=${encodeURIComponent(peer)}`);
  }
  send(kind, body, idempotencyKey) {
    if (!idempotencyKey) throw new Error("Supply a stable unique key for this intended message");
    return this.request(`/v1/messages/${encodeURIComponent(kind)}`, { method: "POST", body, idempotencyKey });
  }
  download(messageID) {
    return this.request(`/v1/media/${encodeURIComponent(messageID)}/download`, { method: "POST" });
  }
  async media(messageID) {
    const response = await fetch(`${this.baseURL}/v1/media/${encodeURIComponent(messageID)}`, {
      headers: { Authorization: `Bearer ${this.token}` },
      signal: AbortSignal.timeout(180000),
    });
    if (!response.ok) throw new Error(`Media download returned HTTP ${response.status}`);
    return new Uint8Array(await response.arrayBuffer());
  }
}
