#!/usr/bin/env bash
# Part B 公共运行库。只读仓库 / 只写 $WORK；绝不 git add/commit、绝不动 static/admin.html。
set -uo pipefail
export PATH=$PATH:/usr/local/go/bin
WORK=${WORK:-/tmp/t41b}
REPO=${REPO:-/home/jingyx/project/opencode2api-test}
FIXTURE=${FIXTURE:-$WORK/fixture}
FROZEN=${FROZEN:-$WORK/frozen}          # 冻结时的仓库副本（构建/变异都在这上面跑）
# 复用仓库里已热的 Go 缓存（5GB），避免 Part B 重新下载/编译；只在 gitignore 目录里写
# 注意：调用方 shell 里若 GOCACHE 是空/不可写（常见于同一行 export 里引用未赋值变量），这里兜底
gocache_ok() { [ -n "${1:-}" ] && mkdir -p "$1" 2>/dev/null && [ -w "$1" ]; }
gocache_ok "${GOCACHE:-}" || GOCACHE="$REPO/.gocache"
gocache_ok "${GOMODCACHE:-}" || GOMODCACHE="$REPO/.modcache"
GOTMPDIR=${GOTMPDIR:-$WORK/.gotmp}; mkdir -p "$GOTMPDIR" 2>/dev/null
export GOCACHE GOMODCACHE GOTMPDIR
[ -w "$GOCACHE" ] || { echo "GOCACHE 不可写: $GOCACHE" >&2; exit 1; }
mkdir -p "$WORK"/{bin,logs,results} "$GOTMPDIR"

# 冻结清单：task-39 可能触碰的文件 + 面板红线文件
# 冻结范围以 Lead 的权威 pin 文件为准（81 行 = 仓库根全部 *.go 80 个 + free_models_cache.json）。
# 面板 static/admin.html 已被 Lead 明确排除（并发面板任务在改），不在本次验收范围。
LEAD_PIN=${LEAD_PIN:-/tmp/t41b/pin_lead.txt}
LEAD_PIN_SHA=${LEAD_PIN_SHA:-5f7158de4ace768ed0917a5d15624f4f3805722a03b8eae71caf4c2ae2203e1e}
pin_file_ok()  { [ -f "$LEAD_PIN" ] && [ "$(sha256sum "$LEAD_PIN" | awk '{print $1}')" = "$LEAD_PIN_SHA" ]; }
pin_list()     { ( cd "$REPO" && awk '{print $2}' "$LEAD_PIN" | xargs sha256sum | sort -k2 ); }
pin_check()    { ( cd "$REPO" && sha256sum -c "$LEAD_PIN" ); }          # 逐文件校验
pin_save()     { pin_list > "$WORK/pin.$1.txt"; date -Is > "$WORK/pin.$1.stamp"; }
pin_verify()   { diff -q <(pin_list) "$WORK/pin.$1.txt" >/dev/null 2>&1; }  # 与本轮冻结快照比
pin_show()     { pin_list; }

build_from() { # $1=src_dir $2=tag -> $WORK/bin/$2
  ( cd "$1" && go build -o "$WORK/bin/$2" . ) >"$WORK/logs/build.$2.log" 2>&1 \
    && echo "built $2" || { echo "BUILD FAILED $2"; tail -20 "$WORK/logs/build.$2.log"; return 1; }
}

# old 基线 = git HEAD 的干净快照（task-21 口径，无 everServed/无 F1 过滤）
build_old_from_head() {
  rm -rf "$WORK/oldsrc"; mkdir -p "$WORK/oldsrc"
  ( cd "$REPO" && git archive HEAD ) | tar -x -C "$WORK/oldsrc" || return 1
  ( cd "$WORK/oldsrc" && grep -q "everServed" main.go ) && { echo "警告: HEAD 已含 everServed，old 基线不纯"; return 2; }
  build_from "$WORK/oldsrc" old
}

tcp_up() { ( exec 3<>"/dev/tcp/127.0.0.1/$1" ) 2>/dev/null; }

free_port() {
  local p
  for p in $(seq 9100 9899); do tcp_up "$p" || { echo "$p"; return 0; }; done
  return 1
}

# 起服务：黑名单代理 + 空密码 + 独立 cwd（日志/缓存都落在 fixture 里，不碰仓库）
start_server() { # $1=bin_tag $2=port $3=dir
  local tag=$1 port=$2 dir=$3
  ( cd "$dir" && env -u ALL_PROXY -u all_proxy \
      HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
      http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 NO_PROXY= no_proxy= \
      "$WORK/bin/$tag" -port "$port" -config "$dir/config.json" -password "" \
      -log-file "$dir/server-$port.log" -log-stdout false \
      >"$dir/server-$port.out" 2>&1 & echo $! >"$dir/server-$port.pid" )
  local i
  for i in $(seq 1 400); do tcp_up "$port" && return 0; sleep 0.05; done
  echo "SERVER START TIMEOUT $tag:$port" >&2; tail -5 "$dir/server-$port.out" >&2; return 1
}

stop_server() { # $1=port $2=dir
  local pidfile="$2/server-$1.pid" pid
  [ -f "$pidfile" ] || return 0
  pid=$(cat "$pidfile"); kill "$pid" 2>/dev/null
  local i; for i in $(seq 1 200); do kill -0 "$pid" 2>/dev/null || break; sleep 0.05; done
  kill -9 "$pid" 2>/dev/null; rm -f "$pidfile"
}

http_get() { # $1=port $2=path $3=outfile -> stdout 状态码
  curl -s -o "$3" -w '%{http_code}' --max-time 120 "http://127.0.0.1:$1$2"
}

http_post_json() { # $1=port $2=path $3=body $4=outfile -> 状态码
  curl -s -o "$4" -w '%{http_code}' --max-time 120 -X POST \
    -H 'Content-Type: application/json' --data-binary "$3" "http://127.0.0.1:$1$2"
}

json_field() { # $1=file $2=python expr using d
  python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print($2)" "$1"
}
