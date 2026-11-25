// sorus Service Worker
// 通常の /static/ 要求は ?v=<内容のハッシュ> を含む URL ごとに保存し、旧版を整理する。
// オフライン画面だけをインストール時に事前キャッシュする。
const CACHE_NAME = 'sorus-v3';
const PRECACHE_URLS = ['/admin/offline'];

self.addEventListener('install', function(event) {
  event.waitUntil(
    caches.open(CACHE_NAME).then(function(cache) {
      return Promise.all(PRECACHE_URLS.map(function(url) { return cache.add(url).catch(function() {}); }));
    }).then(function() {
      return self.skipWaiting();
    })
  );
});

self.addEventListener('activate', function(event) {
  event.waitUntil(
    caches.keys().then(function(keys) {
      return Promise.all(
        keys.filter(function(key) { return key !== CACHE_NAME; }).map(function(key) { return caches.delete(key); })
      );
    }).then(function() {
      return self.clients.claim();
    })
  );
});

self.addEventListener('fetch', function(event) {
  if (event.request.method !== 'GET') {
    return;
  }
  const url = new URL(event.request.url);
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetch(event.request).catch(function() {
        return url.pathname === '/admin' || url.pathname.startsWith('/admin/') ? caches.match('/admin/offline') : Promise.reject(new Error('offline'));
      })
    );
    return;
  }
  if (url.pathname.startsWith('/static/')) {
    event.respondWith(
      caches.match(event.request).then(function(cached) {
        return cached || fetch(event.request).then(function(response) {
          if (response.status === 200) {
            const clone = response.clone();
            caches.open(CACHE_NAME).then(function(cache) {
              return cache.put(event.request, clone).then(function() {
                return pruneOldVersions(cache, url);
              });
            });
          }
          return response;
        }).catch(function() {
          // 未キャッシュの静的ファイルは通信断時に利用できない。
          return caches.match(url.origin + url.pathname);
        });
      })
    );
    return;
  }
  event.respondWith(
    fetch(event.request).catch(function() {
      return caches.match(event.request).then(function(cached) {
        return cached || Promise.reject(new Error('offline'));
      });
    })
  );
});

// pruneOldVersions は url と同じファイルの古い版（?v= の値が違うもの）をキャッシュから捨てる。
function pruneOldVersions(cache, url) {
  if (!url.searchParams.has('v')) {
    return Promise.resolve();
  }
  return cache.keys().then(function(keys) {
    return Promise.all(keys.filter(function(key) {
      const cached = new URL(key.url);
      return cached.pathname === url.pathname && cached.searchParams.has('v') && cached.search !== url.search;
    }).map(function(key) { return cache.delete(key); }));
  });
}
