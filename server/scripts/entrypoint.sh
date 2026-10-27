#!/bin/sh
# venera-nas 容器入口。
#
# 为什么需要它：镜像构建时已经 `chown venera:venera /data /library`，
# 但用户用 `-v ./data:/data` 挂载时，**宿主机目录会盖住镜像里的这层权限**。
# 宿主机目录通常属于 root，而容器原先写死 `USER venera`（UID 1000），
# 于是 SQLite 连库文件都建不出来：
#
#   open store: migrate "CREATE TABLE IF NOT EXISTS sources (": unable to open database file (14)
#
# 这条消息完全不提权限，极难排查。所以这里以 root 起步，先把挂载点的
# 属主修好，再降权执行真正的二进制。
#
# 两种启动方式都支持：
#   - 默认（root）：修权限 + su-exec 降权到 venera
#   - compose 里写了 `user: "1000:1000"`：已经是非 root，跳过 chown 直接跑
set -e

APP_USER="${VENERA_APP_USER:-venera}"
DATA_DIR="${VENERA_DATA_DIR:-/data}"
LIBRARY_DIR="${VENERA_LIBRARY_DIR:-/library}"

if [ "$(id -u)" = "0" ]; then
    # 只改目录本身的属主，不递归：
    # library 下可能有几十万张图，递归 chown 会让每次启动卡几分钟。
    # 子文件由应用创建，天然就是 venera 的。
    for d in "$DATA_DIR" "$LIBRARY_DIR"; do
        if [ -d "$d" ]; then
            owner="$(stat -c '%U' "$d" 2>/dev/null || echo unknown)"
            if [ "$owner" != "$APP_USER" ]; then
                echo "[entrypoint] fixing ownership of $d (was $owner) -> $APP_USER" >&2
                chown "$APP_USER:$APP_USER" "$d" 2>/dev/null || \
                    echo "[entrypoint] WARN: chown $d failed (read-only mount?)" >&2
            fi
        fi
    done

    # 先自检：以 venera 身份能否在 data 目录建文件。
    # 不能的话直接把原因打出来，而不是让 SQLite 抛那句看不懂的 (14)。
    if [ -d "$DATA_DIR" ] && ! su-exec "$APP_USER" sh -c ": > '$DATA_DIR/.entrypoint-writecheck'" 2>/dev/null; then
        echo "[entrypoint] FATAL: $DATA_DIR is not writable by $APP_USER." >&2
        echo "[entrypoint] Mounted host directories must be owned by the container user." >&2
        echo "[entrypoint] Fix it on the host:" >&2
        echo "[entrypoint]   sudo chown -R 1000:1000 <host-path-for-/data>" >&2
        echo "[entrypoint] Or run the container as root by setting: user: \"0:0\"" >&2
        exit 1
    fi
    su-exec "$APP_USER" sh -c ": > '$DATA_DIR/.entrypoint-writecheck' 2>/dev/null && rm -f '$DATA_DIR/.entrypoint-writecheck'" || true

    exec su-exec "$APP_USER:$APP_USER" /usr/local/bin/venera-nas "$@"
fi

# 非 root 启动：不动权限，直接跑。写不进去时由应用层的
# checkWritable 给出带 chown 提示的错误。
exec /usr/local/bin/venera-nas "$@"
