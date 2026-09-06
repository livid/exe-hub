// The hub's service worker: push only. It shows the notification the hub
// sent and opens the page on a click. There is no fetch handler, so
// nothing is ever served from a cache — the pages stay live views of
// the hub.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", e => e.waitUntil(self.clients.claim()));
self.addEventListener("push", e => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch (err) {}
  e.waitUntil(self.registration.showNotification(m.title || "New post", {
    body: m.body || "", icon: "/icon-192.png", tag: m.tag, data: { url: m.url || "/" } }));
});
self.addEventListener("notificationclick", e => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || "/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(list => {
    const open = list.find(c => c.url === url);
    return open ? open.focus() : self.clients.openWindow(url);
  }));
});
