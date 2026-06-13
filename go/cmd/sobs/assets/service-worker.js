self.addEventListener('push', function (event) {
    var data = {};
    try {
        data = event.data ? event.data.json() : {};
    } catch (_err) {
        data = { title: 'SOBS Alert', body: event.data ? event.data.text() : 'Notification received' };
    }

    var title = (data && data.title) || 'SOBS Alert';
    var options = {
        body: (data && data.body) || 'Notification received',
    };

    event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', function (event) {
    event.notification.close();
    event.waitUntil(clients.openWindow(self.registration.scope));
});
