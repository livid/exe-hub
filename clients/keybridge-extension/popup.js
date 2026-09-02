const state = {
  walletAddress: "",
  author: "",
  lastSignature: ""
};

const $ = (id) => document.getElementById(id);
const hubUrl = $("hub-url");
const operation = $("operation");
const payloadText = $("payload-text");
const payloadLabel = $("payload-label");
const sequence = $("sequence");
const messagePreview = $("message-preview");
const signButton = $("sign-button");
const connectButton = $("connect-button");
const feedback = $("feedback");
const resultPanel = $("result-panel");
const signatureOutput = $("signature-output");
const copyButton = $("copy-button");

const BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

function setFeedback(text, kind = "") {
  feedback.textContent = text;
  feedback.className = `feedback ${kind}`.trim();
}

function bytesToBase64(bytes) {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.slice(i, i + 0x8000));
  }
  return btoa(binary);
}

async function verifyEd25519(message, signatureBytes, publicKeyBytes) {
  const key = await crypto.subtle.importKey(
    "raw",
    new Uint8Array(publicKeyBytes),
    { name: "Ed25519" },
    false,
    ["verify"]
  );
  return crypto.subtle.verify(
    { name: "Ed25519" },
    key,
    new Uint8Array(signatureBytes),
    new TextEncoder().encode(message)
  );
}

function base58ToBytes(value) {
  let number = 0n;
  for (const char of value) {
    const digit = BASE58.indexOf(char);
    if (digit < 0) throw new Error("Phantom 返回了无效的 Solana 公钥");
    number = number * 58n + BigInt(digit);
  }

  let hex = number.toString(16);
  if (hex.length % 2) hex = `0${hex}`;
  const decoded = hex ? new Uint8Array(hex.match(/.{2}/g).map((pair) => parseInt(pair, 16))) : new Uint8Array();
  const leadingZeros = value.match(/^1*/)[0].length;
  const result = new Uint8Array([...new Uint8Array(leadingZeros), ...decoded]);
  if (result.length !== 32) throw new Error("Phantom 公钥长度异常");
  return result;
}

function normalizeHubUrl() {
  const value = hubUrl.value.trim().replace(/\/+$/, "");
  if (!/^https:\/\/hub\.v2core\.com$/i.test(value)) {
    throw new Error("当前版本仅允许使用 https://hub.v2core.com");
  }
  return value;
}

function bodyForOperation() {
  const text = payloadText.value;
  if (!text.trim()) throw new Error("请先输入内容");
  if (operation.value === "profile.set") return { name: text.trim() };
  return { text };
}

async function getNextSequence(base, author) {
  const response = await fetch(`${base}/v1/seq?author=${encodeURIComponent(author)}`);
  if (!response.ok) throw new Error(`读取 Hub 序号失败（${response.status}）`);
  const data = await response.json();
  return Number(data.seq) + 1;
}

function buildEnvelope(seq, body) {
  return JSON.stringify({
    type: operation.value,
    author: state.author,
    seq,
    ts: Date.now(),
    body
  });
}

function updateFormCopy() {
  payloadLabel.textContent = operation.value === "profile.set" ? "显示名称" : "帖子内容";
  payloadText.placeholder = operation.value === "profile.set" ? "例如：Tang Xiaoping" : "输入要发布的内容";
  payloadText.value = operation.value === "profile.set" ? "Tang Xiaoping" : "";
  resultPanel.hidden = true;
  messagePreview.textContent = "连接 Phantom 后生成精确消息";
  sequence.value = "自动获取";
}

async function sendRuntimeMessage(message) {
  const response = await chrome.runtime.sendMessage(message);
  if (!response?.ok) throw new Error(response?.error || "插件请求失败");
  return response.result;
}

async function connect() {
  connectButton.disabled = true;
  setFeedback("正在请求 Phantom 连接…");
  try {
    const result = await sendRuntimeMessage({ type: "connectPhantom" });
    state.walletAddress = result.publicKey;
    state.author = bytesToBase64([...base58ToBytes(state.walletAddress)]);
    $("status-light").classList.add("connected");
    $("connection-title").textContent = "Phantom 已连接";
    $("connection-address").textContent = state.walletAddress;
    signButton.disabled = false;
    setFeedback("钱包已连接，可以生成签名。", "success");
  } catch (error) {
    setFeedback(error.message, "error");
  } finally {
    connectButton.disabled = false;
  }
}

async function sign() {
  signButton.disabled = true;
  resultPanel.hidden = true;
  try {
    const base = normalizeHubUrl();
    const seq = await getNextSequence(base, state.author);
    const envelope = buildEnvelope(seq, bodyForOperation());
    const message = `exe-hub:v1\n${envelope}`;
    sequence.value = String(seq);
    messagePreview.textContent = message;
    setFeedback("请在 Phantom 弹窗中确认签名…");

    const result = await sendRuntimeMessage({
      type: "signWithPhantom",
      message,
      expectedAddress: state.walletAddress
    });

    const signatureBase64 = bytesToBase64(result.signatureBytes);
    const valid = await verifyEd25519(
      message,
      result.signatureBytes,
      base58ToBytes(state.walletAddress)
    );
    if (!valid) throw new Error("签名校验失败，未复制任何内容");

    state.lastSignature = signatureBase64;
    signatureOutput.value = signatureBase64;
    resultPanel.hidden = false;
    setFeedback("签名已生成并通过本地校验，请点击“复制签名”。", "success");
  } catch (error) {
    setFeedback(error.message, "error");
  } finally {
    signButton.disabled = false;
  }
}

async function copySignature() {
  if (!state.lastSignature) return;
  try {
    await navigator.clipboard.writeText(state.lastSignature);
    setFeedback("签名已复制。", "success");
  } catch {
    signatureOutput.focus();
    signatureOutput.select();
    setFeedback("浏览器不允许自动复制，请手动复制选中文本。", "error");
  }
}

operation.addEventListener("change", updateFormCopy);
connectButton.addEventListener("click", connect);
signButton.addEventListener("click", sign);
copyButton.addEventListener("click", copySignature);
updateFormCopy();
