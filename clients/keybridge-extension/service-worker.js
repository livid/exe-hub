function findActiveTab() {
  return chrome.tabs.query({ active: true, lastFocusedWindow: true }).then((tabs) => {
    const tab = tabs[0];
    if (!tab?.id) throw new Error("找不到当前浏览器页面");
    if (!tab.url?.startsWith("https://hub.v2core.com/")) {
      throw new Error("请先打开 https://hub.v2core.com/ 再使用 Keybridge");
    }
    return tab;
  });
}

async function runInPage(func, args = []) {
  const tab = await findActiveTab();
  const results = await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    world: "MAIN",
    func,
    args
  });
  return results[0]?.result;
}

async function connectPhantom() {
  return runInPage(async () => {
    const provider = window.phantom?.solana || window.solana;
    if (!provider) throw new Error("当前页面没有检测到 Phantom");
    const response = await provider.connect();
    const publicKey = provider.publicKey?.toString() || response?.publicKey?.toString();
    if (!publicKey) throw new Error("Phantom 未返回公钥");
    return { publicKey };
  });
}

async function signWithPhantom(message, expectedAddress) {
  return runInPage(async (signingMessage, address) => {
    const provider = window.phantom?.solana || window.solana;
    if (!provider) throw new Error("当前页面没有检测到 Phantom");

    await provider.connect();
    const actualAddress = provider.publicKey?.toString();
    if (actualAddress !== address) {
      throw new Error(`Phantom 地址不匹配：${actualAddress || "未知"}`);
    }

    const result = await provider.signMessage(
      new TextEncoder().encode(signingMessage),
      "utf8"
    );
    const signature = result.signature ?? result;
    return {
      publicKey: actualAddress,
      signatureBytes: Array.from(signature)
    };
  }, [message, expectedAddress]);
}

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message.type === "connectPhantom") {
    connectPhantom()
      .then((result) => sendResponse({ ok: true, result }))
      .catch((error) => sendResponse({ ok: false, error: error.message }));
    return true;
  }

  if (message.type === "signWithPhantom") {
    signWithPhantom(message.message, message.expectedAddress)
      .then((result) => sendResponse({ ok: true, result }))
      .catch((error) => sendResponse({ ok: false, error: error.message }));
    return true;
  }

  return false;
});
