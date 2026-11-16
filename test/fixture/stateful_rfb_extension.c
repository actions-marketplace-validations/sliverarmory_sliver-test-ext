/* A small stateful Sliver extension with a deterministic RFB 3.8 endpoint. */

#include <arpa/inet.h>
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#include <string.h>

typedef int32_t (*sliver_callback)(char *, int32_t);

#define SLIVER_EXPORT __attribute__((visibility("default")))

static pthread_mutex_t state_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_t server_thread;
static int listener_fd = -1;
static int server_running;
static int initialized;

static int send_all(int fd, const void *data, size_t length) {
    const unsigned char *bytes = (const unsigned char *)data;
    while (length != 0) {
        ssize_t sent = send(fd, bytes, length, MSG_NOSIGNAL);
        if (sent < 0 && errno == EINTR) {
            continue;
        }
        if (sent <= 0) {
            return -1;
        }
        bytes += sent;
        length -= (size_t)sent;
    }
    return 0;
}

static int receive_all(int fd, void *data, size_t length) {
    unsigned char *bytes = (unsigned char *)data;
    while (length != 0) {
        ssize_t received = recv(fd, bytes, length, 0);
        if (received < 0 && errno == EINTR) {
            continue;
        }
        if (received <= 0) {
            return -1;
        }
        bytes += received;
        length -= (size_t)received;
    }
    return 0;
}

static void serve_client(int fd) {
    static const char banner[] = "RFB 003.008\n";
    static const unsigned char security_types[] = {1, 1};
    static const unsigned char security_result[] = {0, 0, 0, 0};
    static const unsigned char server_init[] = {
        0, 2, 0, 2,          /* 2 x 2 framebuffer. */
        32, 24, 0, 1,        /* 32 bpp, 24 depth, little-endian true color. */
        0, 255, 0, 255, 0, 255,
        16, 8, 0, 0, 0, 0,  /* RGB max values and shifts, then padding. */
        0, 0, 0, 14          /* Desktop name length. */
    };
    static const char desktop_name[] = "sliver-ext-e2e";
    static const unsigned char update_header[] = {
        0, 0, 0, 1,          /* FramebufferUpdate, one rectangle. */
        0, 0, 0, 0, 0, 2, 0, 2,
        0, 0, 0, 0           /* Raw encoding. */
    };
    static const unsigned char pixels[] = {
        0, 0, 255, 0,       /* red */
        0, 255, 0, 0,       /* green */
        255, 0, 0, 0,       /* blue */
        255, 255, 255, 0    /* white */
    };
    unsigned char received[12];
    struct timeval timeout = {.tv_sec = 2, .tv_usec = 0};
    (void)setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    (void)setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));

    if (send_all(fd, banner, sizeof(banner) - 1) != 0 ||
        receive_all(fd, received, sizeof(banner) - 1) != 0 ||
        memcmp(received, banner, sizeof(banner) - 1) != 0 ||
        send_all(fd, security_types, sizeof(security_types)) != 0 ||
        receive_all(fd, received, 1) != 0 || received[0] != 1 ||
        send_all(fd, security_result, sizeof(security_result)) != 0 ||
        receive_all(fd, received, 1) != 0 ||
        send_all(fd, server_init, sizeof(server_init)) != 0 ||
        send_all(fd, desktop_name, sizeof(desktop_name) - 1) != 0 ||
        receive_all(fd, received, 10) != 0 || received[0] != 3 ||
        send_all(fd, update_header, sizeof(update_header)) != 0) {
        return;
    }
    (void)send_all(fd, pixels, sizeof(pixels));
}

static void *serve_rfb(void *unused) {
    int fd = (int)(intptr_t)unused;
    for (;;) {
        int client = accept(fd, NULL, NULL);
        if (client < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }
        serve_client(client);
        close(client);
    }
    return NULL;
}

static int parse_start_port(const char *input, size_t length) {
    static const char prefix[] = "start --port ";
    int port = 0;
    size_t index;
    if (length <= sizeof(prefix) - 1 ||
        memcmp(input, prefix, sizeof(prefix) - 1) != 0) {
        return 0;
    }
    for (index = sizeof(prefix) - 1; index < length; ++index) {
        unsigned char digit = (unsigned char)input[index];
        if (digit < '0' || digit > '9') {
            return 0;
        }
        port = port * 10 + (digit - '0');
        if (port > 65535) {
            return 0;
        }
    }
    return port;
}

static const char *start_server(int port) {
    struct sockaddr_in address;
    int fd;
    int reuse = 1;
    pthread_mutex_lock(&state_lock);
    if (server_running) {
        pthread_mutex_unlock(&state_lock);
        return "already-running";
    }
    fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        pthread_mutex_unlock(&state_lock);
        return "socket-error";
    }
    (void)setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, sizeof(reuse));
    memset(&address, 0, sizeof(address));
    address.sin_family = AF_INET;
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    address.sin_port = htons((uint16_t)port);
    if (bind(fd, (const struct sockaddr *)&address, sizeof(address)) != 0 ||
        listen(fd, 8) != 0) {
        close(fd);
        pthread_mutex_unlock(&state_lock);
        return "bind-error";
    }
    listener_fd = fd;
    server_running = 1;
    if (pthread_create(&server_thread, NULL, serve_rfb, (void *)(intptr_t)fd) != 0) {
        server_running = 0;
        listener_fd = -1;
        close(fd);
        pthread_mutex_unlock(&state_lock);
        return "thread-error";
    }
    pthread_mutex_unlock(&state_lock);
    return "started";
}

static const char *stop_server(void) {
    pthread_t thread;
    int fd;
    pthread_mutex_lock(&state_lock);
    if (!server_running) {
        pthread_mutex_unlock(&state_lock);
        return "not-running";
    }
    fd = listener_fd;
    thread = server_thread;
    server_running = 0;
    listener_fd = -1;
    pthread_mutex_unlock(&state_lock);
    (void)shutdown(fd, SHUT_RDWR);
    close(fd);
    (void)pthread_join(thread, NULL);
    return "stopped";
}

SLIVER_EXPORT int32_t FixtureInit(void) {
    initialized = 1;
    return 0;
}

SLIVER_EXPORT int32_t FixtureControl(char *input, uint32_t input_size,
                                     sliver_callback callback) {
    const char *output;
    int port;
    if (callback == NULL || (input == NULL && input_size != 0)) {
        return -1;
    }
    if (!initialized) {
        output = "not-initialized";
    } else if (input_size == 6 && memcmp(input, "status", 6) == 0) {
        pthread_mutex_lock(&state_lock);
        output = server_running ? "running" : "stopped";
        pthread_mutex_unlock(&state_lock);
    } else if (input_size == 4 && memcmp(input, "stop", 4) == 0) {
        output = stop_server();
    } else if ((port = parse_start_port(input, input_size)) != 0) {
        output = start_server(port);
    } else {
        output = "invalid-command";
    }
    return callback((char *)output, (int32_t)strlen(output));
}
