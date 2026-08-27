#!/usr/bin/env bash
# One-shot Vana network pipeline:
#   Deneb on Prysm v5.1.0  ->  seeded deposit contract  ->  patched Prysm + contract
#   switch  ->  Spectra (Electra/Prague)  ->  Fusaka (Fulu/Osaka)
#
#   ./deploy-pipeline.sh [--mode shadowfork|standard] [--stacks 1|2]
#                        [--start-at N] [--stop-after N] [--yes]
#
# --stacks 2 runs the client swap as a two-stack migration: stack 2 (patched Prysm
# on a cloned chaindata) joins the same chain as a peer, proves itself by proposing
# with 8 of the 32 keys, and only then do the other 24 migrate. Keys move
# stop-then-start - the same key must never run twice - so some slots go unproposed
# during the cutover; the script counts exactly how many instead of glossing over it.
#
# Every phase VERIFIES before the next begins; a failed gate aborts with a reason
# rather than leaving a half-built network that looks healthy. Each hard-won
# lesson is enforced here rather than left to the operator:
#
#  * geth starts ALONE and the head is aligned once, before any CL exists.
#  * eth1_deposit_index == deposit_count, or the chain wedges on block 1.
#  * --contract-deployment-block is the ORIGINAL deployment (25), never the fork
#    block; the fork block yields an empty trie that rejects every deposit log
#    while the chain still runs and finalizes.
#  * the generator runs in `cl` mode with the real genesis pre-placed ('all'
#    silently rewrites it as chainId 1337) and wants the FULL JSON-RPC envelope.
#  * fork epochs -> 0 or the genesis comes out phase0 with a null payload header.
#  * SLOT_DURATION_MS must agree with SECONDS_PER_SLOT (newer Prysm defaults to
#    12000ms; v5.1.0 ignores the field entirely).
#  * client swaps use --sync-from head; without it Prysm resumes from the
#    justified checkpoint, leaving the EL ahead and deadlocking a peerless fork.
#  * Prague needs EIP-2935/7002/7251 deployed BEFORE pragueTime or geth's system
#    calls fail; they go in via canonical presigned txs (--rpc.allow-unprotected-txs).
#  * VC datadirs are wiped on genesis rebuilds: genesis_validators_root is stable,
#    so a stale slashing DB looks valid and silently refuses to sign.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
WORK="$PWD"

MODE=shadowfork
STACKS=1
START_AT=1
STOP_AFTER=8
ASSUME_YES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --mode) MODE="$2"; shift 2 ;;
    --stop-after) STOP_AFTER="$2"; shift 2 ;;
    --start-at) START_AT="$2"; shift 2 ;;
    --stacks) STACKS="$2"; shift 2
      case "$STACKS" in 1|2|3|4) ;; *) echo "--stacks must be 1..4"; exit 2 ;; esac ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
done

# ---------------------------------------------------------------- configuration
SNAPSHOT_TAR="geth-chaindata-20260628.tar"
SNAPSHOT_MD5="618c97b144357941966d022dde22e6bd"
FORK_BLOCK=8630374                 # state head of that snapshot
CHAIN_ID=1480
GETH_IMG=ethereum/client-go:v1.16.8
GEN_IMG=ethpandaops/ethereum-genesis-generator:6.1.5
CL_V51=gcr.io/prysmaticlabs/prysm/beacon-chain:v5.1.0
VC_V51=gcr.io/prysmaticlabs/prysm/validator:v5.1.0
CL_PATCHED=prysm-beacon:local
VC_NEW=gcr.io/prysmaticlabs/prysm/validator:v7.1.8
FOUNDRY=ghcr.io/foundry-rs/foundry:latest
VALTOOLS=protolambda/eth2-val-tools:latest
RETIRED_DEPOSIT=0x17BbE91c315Bf14f38F6D35052a827cadfFe184e
DEPOSIT_DEPLOY_BLOCK=25            # original deployment; NOT the fork block
FUNDER_KEY=0xbcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31
FUNDER=0x8943545177806ED17B9F23F0a21ee5948eCaa776
# SEEDED_SRC_DIR is the PROJECT ROOT (forge needs it for remappings and deps);
# SEEDED_CONTRACT_PATH is the .sol path relative to it, auto-detected when unset.
SEEDED_SRC_DIR="${SEEDED_SRC_DIR:-$HOME/repos/prysm/dist}"
SEEDED_CONTRACT_PATH="${SEEDED_CONTRACT_PATH:-}"
# used only by the 6-arg permissioned (Ownable2Step) variant
SEEDED_MIN_DEPOSIT="${SEEDED_MIN_DEPOSIT:-35000000000000000000000}"   # 35,000 VANA in wei
SEEDED_OWNER="${SEEDED_OWNER:-$FUNDER}"                               # a key we hold, so we can whitelist
SEEDED_RESTRICTED="${SEEDED_RESTRICTED:-true}"
MNEMONIC="test test test test test test test test test test test junk"
# Endpoints and container names are MUTABLE: in two-stack mode phase 6 repoints
# them at stack 2 after the cutover, so phases 7-8 operate on the surviving stack
# without anything having to restart on "canonical" ports.
EL_NAME=vana-el ; CL_NAME=vana-cl
R=http://127.0.0.1:8545
RIN=http://vana-el:8545
B=http://127.0.0.1:3500
# stack 2 (only used when --stacks 2)
EL2_NAME=vana-el2 ; CL2_NAME=vana-cl2
R2=http://127.0.0.1:8546
RIN2=http://vana-el2:8545
B2=http://127.0.0.1:3501

# ---------------------------------------------------------------- N-stack helpers
# Everything below is indexed by stack number k (1..STACKS) so one code path serves
# any layout. Stack 1 keeps the original names/ports so --stacks 1 is unchanged.
# Rationale for N>=3: no node is ever the only one, so --min-sync-peers 1 stays
# correct everywhere (see beacon-chain/p2p/pubsub.go - a peerless node with 1 cannot
# publish, builds blocks it never imports, and freezes the chain). The client swap is
# then a rolling per-stack restart, with no key movement and no node retirement.
sk_el()      { [ "$1" = 1 ] && echo vana-el || echo "vana-el$1"; }
sk_cl()      { [ "$1" = 1 ] && echo vana-cl || echo "vana-cl$1"; }
sk_eldata()  { [ "$1" = 1 ] && echo data/execution || echo "data$1/execution"; }
sk_elroot()  { [ "$1" = 1 ] && echo data || echo "data$1"; }
sk_cldata()  { [ "$1" = 1 ] && echo cl-data || echo "cl-data$1"; }
sk_rpc()     { echo "http://127.0.0.1:$(( 8544 + $1 ))"; }
sk_rin()     { echo "http://$(sk_el "$1"):8545"; }
sk_beacon()  { echo "http://127.0.0.1:$(( 3499 + $1 ))"; }
sk_elports() { echo "-p $(( 8544 + $1 )):8545 -p $(( 8550 + $1 )):8551"; }
sk_clports() { echo "-p $(( 3499 + $1 )):3500 -p $(( 3999 + $1 )):4000"; }

# Wallets are dealt round-robin so the key weight is as even as the 4x8 wallet layout
# allows. Even weight is what decides whether finality survives a roll: a stack holding
# more than 1/3 of the set takes participation below the 2/3 threshold while it is down,
# which pauses finality (it resumes - it does not break).
#   --stacks 4 -> 8/8/8/8   (25% each: finality never pauses)
#   --stacks 3 -> 16/8/8    (the 16 stack is 50%: finality pauses on that one roll)
#   --stacks 2 -> 16/16
sk_wallets() {
  local k=$1 i out=""
  for i in 0 1 2 3; do
    [ $(( i % STACKS + 1 )) = "$k" ] && out="$out vcw$i"
  done
  echo "$out"
}
sk_keycount() { local w n=0; for w in $(sk_wallets "$1"); do n=$((n+8)); done; echo "$n"; }

# every stack's beacon dials every lower-numbered stack, giving a connected mesh that
# survives any single node restarting (the reason a hub topology is not enough).
# lowest connected-peer count across all beacons - the number that matters, since one
# isolated node is enough to stall its own proposals.
cl_min_peers() {
  local k n min=9999
  for k in $(seq 1 "$STACKS"); do
    n=$(curl -s -m 10 "$(sk_beacon "$k")/eth/v1/node/peer_count" | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["data"]["connected"])
except Exception: print(0)' 2>/dev/null || echo 0)
    [ "${n:-0}" -lt "$min" ] && min=${n:-0}
  done
  echo "$min"
}

cl_peer_args() {
  local k=$1 j args="" ma
  for j in $(seq 1 $((k-1))); do
    ma=$(docker logs "$(sk_cl "$j")" 2>&1 | grep -oE '/ip4/[0-9.]+/tcp/13000/p2p/[A-Za-z0-9]+' | tail -1)
    [ -n "$ma" ] && args="$args --peer $ma"
  done
  echo "$args"
}


# ---------------------------------------------------------------------- helpers
c_hdr()  { printf '\n\033[1;36m=== %s ===\033[0m\n' "$*"; }
ok()     { printf '  \033[32mOK\033[0m   %s\n' "$*"; }
info()   { printf '       %s\n' "$*"; }
die()    { printf '  \033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

rpc() { curl -s -m 30 -X POST -H 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"method\":\"$1\",\"params\":$2,\"id\":1}" "$R"; }
# tolerate an empty / non-JSON body: during startup polls curl returns nothing and
# json.load() would spew a JSONDecodeError traceback on every iteration. Callers
# already treat "" as not-ready, so swallow it.
rpcv(){ rpc "$1" "$2" | python3 -c 'import sys,json
try:
    r=json.load(sys.stdin)
    print(r["result"] if r.get("result") is not None else "")
except Exception:
    print("")' 2>/dev/null; }
elhead(){ local h; h=$(rpcv eth_blockNumber '[]'); [ -n "$h" ] && python3 -c "print(int('$h',16))" || echo 0; }
clslot(){ curl -s -m 10 "$B/eth/v1/beacon/headers/head" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["header"]["message"]["slot"])' 2>/dev/null || echo -1; }
clfin(){  curl -s -m 10 "$B/eth/v1/beacon/states/head/finality_checkpoints" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["finalized"]["epoch"])' 2>/dev/null || echo -1; }
clfork(){ curl -s -m 10 "$B/eth/v1/beacon/states/head/fork" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["current_version"])' 2>/dev/null || echo '?'; }
cast_in(){ docker run --rm --network vana --entrypoint sh "$FOUNDRY" -c "$*" 2>&1 | tr -d '\r'; }
dep_count(){ cast_in "cast call --rpc-url $RIN $1 'get_deposit_count()(bytes)'" \
             | python3 -c 'import sys;s=sys.stdin.read().strip();print(int.from_bytes(bytes.fromhex(s[2:]),"little") if s.startswith("0x") else -1)'; }
dep_root(){ cast_in "cast call --rpc-url $RIN $1 'get_deposit_root()(bytes32)'" | head -1; }

# wait_for <desc> <timeout_s> <shell-condition>
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local t=0
  info "waiting: $desc (timeout ${timeout}s)"
  while ! eval "$@"; do
    sleep 5; t=$((t+5))
    [ "$t" -ge "$timeout" ] && die "timed out waiting for: $desc"
  done
  ok "$desc"
}

confirm() {
  [ "$ASSUME_YES" = 1 ] && return 0
  printf '  \033[1;33m?\033[0m %s [y/N] ' "$1"; read -r a
  case "$a" in y|Y|yes) return 0 ;; *) die "aborted by operator" ;; esac
}

# Docker Desktop's virtiofs can keep serving a deleted inode, so a bind-mount source
# that is rm -rf'd and immediately re-created may appear EMPTY OR ABSENT inside the
# container. Observed twice: geth/prysm failing outright with
#   "mkdir /cldata/blobs: no such file or directory"
# and - much harder to spot - a half-mounted datadir giving prysm a broken libp2p
# keystore, which surfaces as "failed to negotiate security protocol" on every dial
# and looks exactly like a client-version incompatibility.
# Emptying in place keeps the inode, so the mount is always real.
fresh_dir() {
  local d
  for d in "$@"; do
    mkdir -p "$d"
    find "$d" -mindepth 1 -delete 2>/dev/null || true
  done
}

phase_gate() {
  local n="$1"
  [ "$n" -le "$STOP_AFTER" ] || { c_hdr "stopping after phase $STOP_AFTER as requested"; exit 0; }
  [ "$n" -ge "$START_AT" ] || { info "skip phase $n (resuming at $START_AT)"; return 1; }
  return 0
}

# Resuming mid-pipeline means earlier phases' outputs must already exist and the
# network must be up. Check explicitly so a resume fails with a clear reason
# instead of something obscure several steps later.
need_file() { [ -s "$1" ] || die "this phase needs $1 - rerun from an earlier phase (--start-at)"; }
need_running() {
  local c
  for c in "$@"; do
    docker ps --format '{{.Names}}' | grep -qx "$c" || die "this phase needs container '$c' running - rerun from an earlier phase"
  done
}

# ============================================================ phase 0: teardown
p0_teardown() {
  c_hdr "PHASE 0  teardown + wipe derived state"
  local c
  for c in $(docker ps -aq --filter 'name=^vana-'); do docker stop -t 40 "$c" >/dev/null 2>&1; done
  for c in $(docker ps -aq --filter 'name=^vana-'); do docker rm "$c" >/dev/null 2>&1; done
  [ "$(docker ps -a --filter name=vana -q | wc -l | tr -d ' ')" = "0" ] || die "containers still present"
  ok "all vana-* containers removed"
  # backups, the stack-2 clone and scratch dirs are never re-mounted at these paths,
  # so deleting them outright is safe.
  rm -rf cl-data.* vcdata.* vcdata32 vcdata33 data2 data3 data4 pipeline
  # these ARE bind-mounted again later - empty them in place (see fresh_dir).
  fresh_dir cl-data cl-data2 cl-data3 cl-data4 vcdata gen/data/metadata gen/data/parsed
  for d in vcdata-vcw0 vcdata-vcw1 vcdata-vcw2 vcdata-vcw3; do fresh_dir "$d"; done
  rm -f eth1_data.json fork_block.txt watch-pubkeys.txt deposit*.jsonl dep_PUB dep_WC dep_SIG dep_DDR
  mkdir -p pipeline
  ok "derived state wiped (execution chaindata handled in phase 1)"
  docker network create vana >/dev/null 2>&1 || true
}

# ========================================================== phase 1: chain data
p1_chaindata() {
  c_hdr "PHASE 1  execution chaindata (mode: $MODE)"
  if [ "$MODE" = shadowfork ]; then
    [ -f "$SNAPSHOT_TAR" ] || die "missing $SNAPSHOT_TAR"
    info "verifying snapshot md5 (211 GiB, a few minutes)"
    local got; got=$(md5 -q "$SNAPSHOT_TAR")
    [ "$got" = "$SNAPSHOT_MD5" ] || die "md5 mismatch: $got != $SNAPSHOT_MD5"
    ok "md5 verified"
    rm -rf data
    info "extracting"
    tar -xf "$SNAPSHOT_TAR" -C . || die "extraction failed"
    [ -d data/execution/geth/chaindata ] || die "chaindata missing after extract"
    ok "extracted $(du -sh data | awk '{print $1}')"
    local k root
    for k in $(seq 2 "$STACKS"); do
      # Clone now, before any geth touches it: cloning a RUNNING datadir yields headers
      # ahead of blocks and a copied geth.ipc socket. APFS clonefile makes this ~free;
      # other filesystems fall back to a real copy.
      root=$(sk_elroot "$k")
      rm -rf "$root"
      cp -Rc data "$root" 2>/dev/null || cp -R data "$root" || die "chaindata clone $k failed"
      # nodekey would give the nodes the SAME enode id and they would refuse to peer
      # (each sees the other as itself). geth.ipc is a socket and cannot be bound over.
      rm -f "$root/execution/geth/nodekey" "$root/execution/geth.ipc" "$root/execution/geth/LOCK"
      ok "stack $k chaindata cloned from the pristine extract (nodekey/ipc stripped)"
    done
  else
    rm -rf data; mkdir -p data/execution
    [ -f genesis.json ] || die "missing genesis.json"
    docker run --rm -v "$WORK/data/execution":/data -v "$WORK/genesis.json":/g.json:ro \
      "$GETH_IMG" init --datadir /data /g.json >/dev/null 2>&1 || die "geth init failed"
    ok "standard network initialised from genesis.json (no snapshot)"
  fi
}

# ================================================ phase 2: geth alone + align
p2_el_align() {
  c_hdr "PHASE 2  start geth ALONE, align head"
  docker run -d --name vana-el --network vana \
    -v "$WORK/data/execution":/data -v "$WORK/jwtsecret":/jwt:ro \
    -p 8545:8545 -p 8551:8551 "$GETH_IMG" \
    --datadir /data --networkid "$CHAIN_ID" --nodiscover --maxpeers 0 \
    --http --http.addr 0.0.0.0 --http.port 8545 --http.vhosts '*' \
    --http.api eth,net,web3,debug,txpool,admin \
    --authrpc.addr 0.0.0.0 --authrpc.port 8551 --authrpc.vhosts '*' --authrpc.jwtsecret /jwt \
    --syncmode full --miner.gasprice 1000000000 >/dev/null || die "geth start failed"
  wait_for "geth RPC up" 180 '[ "$(elhead)" -gt 0 ] || [ "$MODE" = standard ]'
  [ "$(rpcv net_peerCount '[]')" = "0x0" ] || die "geth has peers; a shadow fork must be isolated"
  ok "peers = 0 (isolated)"

  if [ "$MODE" = shadowfork ]; then
    docker logs "$EL_NAME" 2>&1 | grep -E 'Rewound to block with state' | tail -1 | sed 's/^/       /'
    local hex; hex=$(python3 -c "print(hex($FORK_BLOCK))")
    rpc debug_setHead "[\"$hex\"]" >/dev/null
    sleep 8
    local h; h=$(elhead)
    [ "$h" = "$FORK_BLOCK" ] || die "head is $h, expected $FORK_BLOCK after setHead"
    [ -z "$(rpcv eth_getBlockByNumber "[\"$(python3 -c "print(hex($FORK_BLOCK+1))")\",false]")" ] \
      || die "header chain still ahead of the fork block"
    ok "head aligned to $FORK_BLOCK, header chain truncated"
    local cnt root
    cnt=$(python3 -c "print(int('$(rpcv eth_getStorageAt "[\"$RETIRED_DEPOSIT\",\"0x20\",\"latest\"]")',16))")
    root=$(rpcv eth_call "[{\"to\":\"$RETIRED_DEPOSIT\",\"data\":\"0xc5f2892f\"},\"latest\"]")
    [ "$cnt" -gt 0 ] || die "deposit contract count is 0 at the fork block"
    ok "deposit contract readable: count=$cnt root=${root:0:20}..."
    echo "$cnt" > pipeline/dep_count
    echo "$root" > pipeline/dep_root
  else
    echo 0 > pipeline/dep_count; echo "0x$(printf '0%.0s' $(seq 64))" > pipeline/dep_root
    ok "standard mode: no fork-block alignment needed"
  fi

  local k elnm elrpc hk
  for k in $(seq 2 "$STACKS"); do
    # --ipcdisable avoids the socket problem entirely; --maxpeers 25 lets the stacks
    # mesh so no node is stranded when another restarts. --cache 512 (geth defaults to
    # 1024) halves its resident set, which is what makes 4 stacks fit under Docker's
    # memory ceiling.
    elnm=$(sk_el "$k"); elrpc=$(sk_rpc "$k")
    docker run -d --name "$elnm" --network vana \
      -v "$WORK/$(sk_eldata "$k")":/data -v "$WORK/jwtsecret":/jwt:ro \
      $(sk_elports "$k") "$GETH_IMG" \
      --datadir /data --networkid "$CHAIN_ID" --nodiscover --maxpeers 25 --ipcdisable \
      --cache 512 \
      --http --http.addr 0.0.0.0 --http.port 8545 --http.vhosts '*' \
      --http.api eth,net,web3,debug,txpool,admin \
      --authrpc.addr 0.0.0.0 --authrpc.port 8551 --authrpc.vhosts '*' --authrpc.jwtsecret /jwt \
      --syncmode full --miner.gasprice 1000000000 >/dev/null || die "stack $k geth failed"
    wait_for "stack $k geth up" 300 'curl -s -m 5 -X POST -H "Content-Type: application/json" --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}" '"$elrpc"' >/dev/null 2>&1'
    if [ "$MODE" = shadowfork ]; then
      curl -s -m 30 -X POST -H 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"method\":\"debug_setHead\",\"params\":[\"$(python3 -c "print(hex($FORK_BLOCK))")\"],\"id\":1}" "$elrpc" >/dev/null
      sleep 8
    fi
    hk=$(curl -s -m 15 -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' "$elrpc" | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["result"],16))')
    [ "$hk" = "$(elhead)" ] || die "stack $k head $hk != stack 1 head $(elhead)"
    ok "stack $k execution client aligned at $hk"
  done
  [ "$STACKS" -gt 1 ] && peer_the_els
}

# cross-add the two execution clients as TRUSTED peers (trusted bypasses maxpeers,
# and the advertised 127.0.0.1 must be rewritten to the container IP to dial).
peer_the_els() {
  # Trusted peers bypass maxpeers, and the enode's advertised 127.0.0.1 must be rewritten
  # to the container IP to be dialable. Full mesh, not a hub: a hub taken down for its
  # own rolling restart would partition everyone else.
  local k j nk ek ipk enk seen="" m
  for k in $(seq 1 "$STACKS"); do
    nk=$(sk_el "$k")
    ek=$(curl -s -m 15 -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","method":"admin_nodeInfo","params":[],"id":1}' "$(sk_rpc "$k")" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["enode"])')
    [ -n "$ek" ] || die "stack $k execution client has no enode"
    case " $seen " in *" ${ek:8:40} "*) die "execution clients share a node id - a clone kept nodekey" ;; esac
    seen="$seen ${ek:8:40}"
    ipk=$(docker inspect "$nk" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
    enk=$(python3 -c "import re,sys;print(re.sub(r'@[^:]+:','@$ipk:',sys.argv[1]))" "$ek")
    for j in $(seq 1 "$STACKS"); do
      [ "$j" = "$k" ] && continue
      for m in admin_addTrustedPeer admin_addPeer; do
        curl -s -m 15 -X POST -H 'Content-Type: application/json' --data "{\"jsonrpc\":\"2.0\",\"method\":\"$m\",\"params\":[\"$enk\"],\"id\":1}" "$(sk_rpc "$j")" >/dev/null
      done
    done
  done
  ok "$STACKS execution clients meshed (distinct node ids)"
}

# ============================================ phase 3: Deneb consensus genesis
p3_genesis() {
  c_hdr "PHASE 3  generate Deneb genesis (32 validators)"
  need_running vana-el; need_file pipeline/dep_count; need_file pipeline/dep_root
  local gt fb
  gt=$(( $(date +%s) + 300 ))
  fb=$(elhead)
  # shadow-fork file must be the FULL JSON-RPC envelope
  curl -s -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBlockByNumber\",\"params\":[\"$(python3 -c "print(hex($fb))")\",false],\"id\":1}" \
    "$R" > gen/config/el/latest_block.json
  python3 -c "
import json,sys
d=json.load(open('gen/config/el/latest_block.json'))
assert 'result' in d and d['result'], 'not a JSON-RPC envelope'
b=d['result']; cnt=int(open('pipeline/dep_count').read()); root=open('pipeline/dep_root').read().strip()
json.dump({'deposit_root':root,'deposit_count':cnt,'eth1_deposit_index':cnt,'block_hash':b['hash']},open('eth1_data.json','w'),indent=1)
open('pipeline/fork_block','w').write(str(int(b['number'],16)))
" || die "could not build eth1_data.json"
  python3 - "$gt" "$fb" <<'PY' || die "config patch failed"
import sys,re
gt,fb=sys.argv[1],sys.argv[2]
FF=18446744073709551615
p='gen/config/cl/config.yaml'; c=open(p).read()
c=re.sub(r'^MIN_GENESIS_TIME: \d+','MIN_GENESIS_TIME: %s'%gt,c,flags=re.M)
for f in ('ALTAIR','BELLATRIX','CAPELLA','DENEB'):
    c=re.sub(r'^%s_FORK_EPOCH: \d+'%f,'%s_FORK_EPOCH: 0'%f,c,flags=re.M)
for f in ('ELECTRA','FULU','GLOAS','HEZE'):
    c=re.sub(r'^%s_FORK_EPOCH: \d+'%f,'%s_FORK_EPOCH: %d'%(f,FF),c,flags=re.M)
if 'SLOT_DURATION_MS' not in c: c=c.rstrip()+'\nSLOT_DURATION_MS: 6000\n'
open(p,'w').write(c)
v=open('gen/values.env').read()
v=re.sub(r'export GENESIS_TIMESTAMP=\d+','export GENESIS_TIMESTAMP=%s'%gt,v)
v=re.sub(r'export CL_EXEC_BLOCK=\d+','export CL_EXEC_BLOCK=%s'%fb,v)
open('gen/values.env','w').write(v)
PY
  # SLOT_DURATION_MS must agree with SECONDS_PER_SLOT
  python3 -c "
import re
c=open('gen/config/cl/config.yaml').read()
sps=int(re.search(r'^SECONDS_PER_SLOT: (\d+)',c,re.M).group(1))
ms=int(re.search(r'^SLOT_DURATION_MS: (\d+)',c,re.M).group(1))
assert ms==sps*1000, 'SLOT_DURATION_MS %d != SECONDS_PER_SLOT %d*1000'%(ms,sps)
print('       slot clock consistent: %ds / %dms'%(sps,ms))" || die "slot clock mismatch"
  fresh_dir gen/data/metadata gen/data/parsed
  cp genesis.json gen/data/metadata/genesis.json      # 'cl' mode must not regenerate this
  grep '^export' gen/values.env | sed 's/^export //' > pipeline/genenv
  docker run --rm -u 0 -v "$WORK/gen/config":/config -v "$WORK/gen/data":/data \
    --env-file pipeline/genenv "$GEN_IMG" cl > pipeline/gen.log 2>&1 || die "generator failed (see pipeline/gen.log)"
  grep -q 'genesis version: deneb' pipeline/gen.log || die "genesis is not deneb: $(grep -o 'genesis version: [a-z0-9]*' pipeline/gen.log)"
  grep -q "chainid: $CHAIN_ID" pipeline/gen.log || die "wrong chainId (ran 'all' instead of 'cl'?)"
  ok "genesis: deneb, chainId $CHAIN_ID, 32 validators"
  python3 patch-eth1-data.py > pipeline/patch.log 2>&1 || die "eth1_data patch failed (see pipeline/patch.log)"
  grep -q 'PATCHED OK: YES' pipeline/patch.log || die "eth1_data patch did not verify"
  python3 -c "
import json;e=json.load(open('eth1_data.json'))
assert e['deposit_count']==e['eth1_deposit_index'], 'index != count would wedge the chain'
print('       eth1_deposit_index == deposit_count == %d'%e['deposit_count'])"
  ok "eth1_data patched into genesis.ssz"
}

# ================================================= phase 4: Prysm v5.1.0 start
p4_start_v51() {
  c_hdr "PHASE 4  start Prysm v5.1.0 + validator"
  need_running vana-el; need_file gen/data/metadata/genesis.ssz; need_file pipeline/fork_block
  # Bring EVERY stack up on v5.1.0 before the chain starts producing, so no beacon has
  # to backfill: they all begin at genesis. Started lowest-first because stack k dials
  # stacks 1..k-1, and their multiaddrs only exist once they are listening.
  local k clnm w vcnm
  for k in $(seq 1 "$STACKS"); do
    clnm=$(sk_cl "$k")
    fresh_dir "$(sk_cldata "$k")"
    if [ "$k" -gt 1 ]; then
      wait_for "stack $((k-1)) beacon advertised a multiaddr" 300 \
        '[ -n "$(docker logs '"$(sk_cl $((k-1)))"' 2>&1 | grep -oE "/ip4/[0-9.]+/tcp/13000/p2p/[A-Za-z0-9]+" | tail -1)" ]'
    fi
    # v5.1.0 predates --http-host/--http-port; it uses --grpc-gateway-*.
    # --min-sync-peers 1 everywhere: with N>=2 no node is ever alone, so the publish
    # path always has a peer (see the pubsub.go note in the helpers) AND range-backfill
    # stays enabled. That is the whole reason this layout needs no flag flipping.
    docker run -d --name "$clnm" --network vana \
      -v "$WORK/$(sk_cldata "$k")":/cldata -v "$WORK/gen/data/metadata":/cfg:ro -v "$WORK/jwtsecret":/jwt:ro \
      $(sk_clports "$k") "$CL_V51" \
      --datadir /cldata --chain-config-file /cfg/config.yaml --genesis-state /cfg/genesis.ssz \
      --execution-endpoint "http://$(sk_el "$k"):8551" --jwt-secret /jwt \
      --contract-deployment-block "$DEPOSIT_DEPLOY_BLOCK" \
      --deposit-contract "$RETIRED_DEPOSIT" --accept-terms-of-use \
      --grpc-gateway-host 0.0.0.0 --grpc-gateway-port 3500 --grpc-gateway-corsdomain '*' \
      --rpc-host 0.0.0.0 --rpc-port 4000 --monitoring-host 0.0.0.0 \
      --subscribe-all-subnets $(cl_peer_args "$k") \
      --min-sync-peers 1 --no-discovery >/dev/null || die "stack $k v5.1.0 beacon failed"
    wait_for "stack $k beacon API up" 240 'curl -s -m 5 "'"$(sk_beacon "$k")"'/eth/v1/node/health" >/dev/null 2>&1'
    # one VC per wallet: prysm takes a single --wallet-dir, and the same key running in
    # two places is a slashable double-sign.
    for w in $(sk_wallets "$k"); do
      vcnm="vana-vc-$w"
      fresh_dir "vcdata-$w"
      docker run -d --name "$vcnm" --network vana \
        -v "$WORK/vcdata-$w":/vcdata -v "$WORK/$w/prysm":/wallet -v "$WORK/vcpass.txt":/pass:ro \
        -v "$WORK/gen/data/metadata":/cfg:ro "$VC_V51" \
        --datadir /vcdata --wallet-dir /wallet --wallet-password-file /pass \
        --chain-config-file /cfg/config.yaml --beacon-rpc-provider "$clnm:4000" \
        --suggested-fee-recipient "$FUNDER" --accept-terms-of-use >/dev/null || die "VC $w start failed"
    done
    ok "stack $k: v5.1.0 beacon + $(sk_keycount "$k") keys ($(sk_wallets "$k") )"
  done
  if [ "$STACKS" -gt 1 ]; then
    wait_for "all beacons peered" 420 '[ "$(cl_min_peers)" -ge 1 ]'
    ok "all $STACKS beacons peered (min peer count $(cl_min_peers))"
  fi
  local fb; fb=$(cat pipeline/fork_block)
  wait_for "blocks being produced" 600 '[ "$(elhead)" -gt '"$((fb+4))"' ]'
  # slot tracking must be exact: EL = fork_block + slot
  local el sl; el=$(elhead); sl=$(clslot)
  [ "$((el-fb))" = "$sl" ] || info "note: EL-fork($((el-fb))) != slot($sl) - missed slots at startup are benign"
  [ "$(docker logs "$CL_NAME" 2>&1 | grep -ci 'invalid payload timestamp')" = "0" ] \
    || die "invalid payload timestamp -> SLOT_DURATION_MS / SECONDS_PER_SLOT mismatch"
  ok "no payload-timestamp errors (clock correct)"
  wait_for "finality past epoch 1" 900 '[ "$(clfin)" -gt 1 ]'
  info "finalized epoch $(clfin), fork version $(clfork)"
  if [ "$STACKS" = 2 ]; then
    wait_for "stack 2 head matches stack 1" 900 '[ "$(curl -s -m 10 "$B2/eth/v1/beacon/headers/head" | python3 -c "import sys,json;print(json.load(sys.stdin)[\"data\"][\"root\"])" 2>/dev/null)" = "$(curl -s -m 10 "$B/eth/v1/beacon/headers/head" | python3 -c "import sys,json;print(json.load(sys.stdin)[\"data\"][\"root\"])" 2>/dev/null)" ]'
    wait_for "stack 2 not optimistic (its EL is validating)" 900 '[ "$(curl -s -m 10 "$B2/eth/v1/node/syncing" | python3 -c "import sys,json;print(json.load(sys.stdin)[\"data\"][\"is_optimistic\"])" 2>/dev/null)" = "False" ]'
    wait_for "stack 2 validators attesting" 900 '[ "$(docker logs vana-vc-vcw3 --since 2m 2>&1 | grep -c "Submitted new attestations")" -ge 2 ]'
    ok "both stacks in consensus; patched build is attesting with 8 keys"
  fi
}




p5_deposit_contract() {
  c_hdr "PHASE 5  deploy seeded deposit contract (continues count + branch)"
  need_running vana-el vana-cl
  [ -d "$SEEDED_SRC_DIR" ] || die "SEEDED_SRC_DIR does not exist: $SEEDED_SRC_DIR"
  if [ -z "$SEEDED_CONTRACT_PATH" ]; then
    SEEDED_CONTRACT_PATH=$(cd "$SEEDED_SRC_DIR" && find . -name DepositContractSeeded.sol \
      -not -path './out/*' -not -path './artifacts/*' -not -path './node_modules/*' -not -path './cache*/*' \
      | head -1 | sed 's|^\./||')
  fi
  [ -n "$SEEDED_CONTRACT_PATH" ] || die "DepositContractSeeded.sol not found anywhere under $SEEDED_SRC_DIR"
  [ -f "$SEEDED_SRC_DIR/$SEEDED_CONTRACT_PATH" ] || die "missing $SEEDED_SRC_DIR/$SEEDED_CONTRACT_PATH"
  info "source: $SEEDED_CONTRACT_PATH  (project root: $SEEDED_SRC_DIR)"
  local nargs SEEDED_SRC
  nargs=$(python3 "$WORK/seeded-arity.py" "$SEEDED_SRC_DIR/$SEEDED_CONTRACT_PATH") || die "arity detection failed"
  info "constructor arity: $nargs"
  SEEDED_SRC="$SEEDED_CONTRACT_PATH:DepositContractSeeded"
  local cnt root
  cnt=$(python3 -c "print(int('$(rpcv eth_getStorageAt "[\"$RETIRED_DEPOSIT\",\"0x20\",\"latest\"]")',16))")
  root=$(dep_root "$RETIRED_DEPOSIT")
  info "retired contract: count=$cnt root=${root:0:20}..."
  local br=""
  for i in $(seq 0 31); do
    br="$br$(rpcv eth_getStorageAt "[\"$RETIRED_DEPOSIT\",\"$(printf '0x%x' "$i")\",\"latest\"]"),"
  done
  br="[${br%,}]"
  local cargs
  case "$nargs" in
    2) cargs="$cnt '$br'" ;;
    6) cargs="$cnt '$br' $SEEDED_MIN_DEPOSIT $SEEDED_OWNER $SEEDED_RESTRICTED '[]'"
       info "permissioned variant: minDeposit=$SEEDED_MIN_DEPOSIT owner=$SEEDED_OWNER restricted=$SEEDED_RESTRICTED" ;;
    *) die "unsupported constructor arity $nargs - supply the args for this variant by hand" ;;
  esac
  local out new
  out=$(docker run --rm --network vana -v "$SEEDED_SRC_DIR":/src -w /src "$FOUNDRY" \
    "forge create $SEEDED_SRC --broadcast --private-key $FUNDER_KEY --rpc-url $RIN \
     --gas-limit 6000000 --priority-gas-price 2000000000 --gas-price 3000000000 \
     --constructor-args $cargs" 2>&1)
  new=$(echo "$out" | grep -oE 'Deployed to: 0x[a-fA-F0-9]{40}' | awk '{print $3}')
  [ -n "$new" ] || { echo "$out" | tail -15; die "forge create failed"; }
  ok "deployed at $new"
  # the continuation must be byte-identical, and every branch slot must match:
  # a matching ROOT alone does not prove it (levels whose count-bit is 0 mix
  # zero_hashes and stay inert until the count changes).
  [ "$(dep_count "$new")" = "$cnt" ] || die "new count $(dep_count "$new") != $cnt"
  [ "$(dep_root  "$new")" = "$root" ] || die "root mismatch: $(dep_root "$new") != $root"
  ok "count and root identical"
  local bad=0
  for i in $(seq 0 31); do
    local a b
    a=$(cast_in "cast call --rpc-url $RIN $new 'get_branch(uint256)(bytes32)' $i" | head -1)
    b=$(rpcv eth_getStorageAt "[\"$RETIRED_DEPOSIT\",\"$(printf '0x%x' "$i")\",\"latest\"]")
    [ "$(echo "$a" | tr 'A-F' 'a-f')" = "$(echo "$b" | tr 'A-F' 'a-f')" ] || bad=$((bad+1))
  done
  [ "$bad" = 0 ] || die "$bad of 32 branch slots differ"
  ok "all 32 branch slots identical"
  # deployment block becomes the switch boundary
  local sw
  sw=$(python3 - "$new" <<'PY'
import json,urllib.request,sys
new=sys.argv[1].lower()
def rpc(m,p):
    r=urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8545",
      json.dumps({"jsonrpc":"2.0","method":m,"params":p,"id":1}).encode(),
      {"Content-Type":"application/json"}),timeout=30)
    return json.load(r).get("result")
head=int(rpc("eth_blockNumber",[]),16)
for n in range(head,head-400,-1):
    b=rpc("eth_getBlockByNumber",[hex(n),True])
    if not b: continue
    for t in b["transactions"]:
        if t.get("to") is None:
            rc=rpc("eth_getTransactionReceipt",[t["hash"]])
            if rc and (rc.get("contractAddress") or "").lower()==new:
                print(n); raise SystemExit
raise SystemExit("deployment block not found")
PY
) || die "could not locate the deployment block"
  if [ "$STACKS" = 2 ]; then
    local pre_addr pre_sw; pre_addr=$(cat pipeline/new_contract); pre_sw=$(cat pipeline/switch_block)
    [ "$(echo "$new" | tr 'A-F' 'a-f')" = "$(echo "$pre_addr" | tr 'A-F' 'a-f')" ] \
      || die "deployed at $new but stack 2 was pre-configured for $pre_addr"
    [ "$sw" -lt "$pre_sw" ] || die "deployed at block $sw, at or after the pre-set switch block $pre_sw"
    ok "matches the address stack 2 was pre-configured with; deployed at $sw, switch at $pre_sw"
  else
    echo "$new" > pipeline/new_contract
    echo "$sw"  > pipeline/switch_block
    ok "switch block = $sw (its deployment block)"
  fi
}



# count slots with no block between two slot numbers - the real cost of the cutover
count_missed_slots() {
  local a="$1" b="$2" have=0 miss=0 i
  i=$a
  while [ "$i" -le "$b" ]; do
    if curl -s -m 8 "$B/eth/v2/beacon/blocks/$i" 2>/dev/null | grep -q '"slot"'; then
      have=$((have+1))
    else
      miss=$((miss+1))
    fi
    i=$((i+1))
  done
  info "cutover window slots $a..$b : $have produced, $miss missed"
}

# multiaddrs of every stack except $1 - a rolled node rejoins by dialing all of them,
# so it does not depend on any single node being up.
cl_other_peer_args() {
  local k=$1 j args="" ma
  for j in $(seq 1 "$STACKS"); do
    [ "$j" = "$k" ] && continue
    ma=$(docker logs "$(sk_cl "$j")" 2>&1 | grep -oE '/ip4/[0-9.]+/tcp/13000/p2p/[A-Za-z0-9]+' | tail -1)
    [ -n "$ma" ] && args="$args --peer $ma"
  done
  echo "$args"
}

# Never take a node down unless the REMAINING nodes are demonstrably carrying the chain.
# This is the check that was missing when the old design retired a stack: it is not
# enough that the node being replaced looks fine.
assert_others_healthy() {
  local k=$1 j b sd adv a bslot
  for j in $(seq 1 "$STACKS"); do
    [ "$j" = "$k" ] && continue
    b=$(sk_beacon "$j")
    sd=$(curl -s -m 10 "$b/eth/v1/node/syncing" | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["data"]["sync_distance"])
except Exception: print(9999)' 2>/dev/null || echo 9999)
    [ "${sd:-9999}" -le 2 ] || die "stack $j is $sd slots behind - refusing to take stack $k down"
  done
  # and the chain must actually be advancing right now
  a=$(clslot); sleep 14; bslot=$(clslot)
  [ "$bslot" -gt "$a" ] || die "chain not advancing (slot stuck at $a) - refusing to take stack $k down"
}

# Roll ONE stack from v5.1.0 to the patched build. Keys never move: this stack's own
# validators go idle for the restart and come back on the same wallets, so there is no
# double-sign window and no slashing-protection hand-off.
roll_stack() {
  local k=$1 newc=$2 swb=$3 clnm w b
  clnm=$(sk_cl "$k"); b=$(sk_beacon "$k")
  info "rolling stack $k ($(sk_keycount "$k") of 32 keys) - the other $((STACKS-1)) stacks keep producing"
  assert_others_healthy "$k"
  # stop this stack's VCs first, so its keys are idle before its beacon disappears
  for w in $(sk_wallets "$k"); do
    docker stop -t 20 "vana-vc-$w" >/dev/null 2>&1; docker rm "vana-vc-$w" >/dev/null 2>&1
  done
  docker stop -t 40 "$clnm" >/dev/null 2>&1; docker rm "$clnm" >/dev/null 2>&1
  # --clear-deposit-contract: the DB records the retired address and the patched build
  # refuses to start on the mismatch. --sync-from head: resume at the real head instead
  # of the justified checkpoint, which would silently concede 8-16 slots.
  # The patched build uses --http-* where v5.1.0 used --grpc-gateway-*.
  docker run -d --name "$clnm" --network vana \
    -v "$WORK/$(sk_cldata "$k")":/cldata -v "$WORK/gen/data/metadata":/cfg:ro -v "$WORK/jwtsecret":/jwt:ro \
    $(sk_clports "$k") "$CL_PATCHED" \
    --datadir /cldata --chain-config-file /cfg/config.yaml --genesis-state /cfg/genesis.ssz \
    --execution-endpoint "http://$(sk_el "$k"):8551" --jwt-secret /jwt \
    --contract-deployment-block "$DEPOSIT_DEPLOY_BLOCK" \
    --deposit-contract "$newc" --retired-deposit-contract "$RETIRED_DEPOSIT" \
    --deposit-contract-switch-block "$swb" --clear-deposit-contract --sync-from head \
    --accept-terms-of-use --subscribe-all-subnets $(cl_other_peer_args "$k") \
    --http-host 0.0.0.0 --http-port 3500 --http-cors-domain '*' \
    --rpc-host 0.0.0.0 --rpc-port 4000 --monitoring-host 0.0.0.0 \
    --min-sync-peers 1 --no-discovery >/dev/null || die "stack $k patched beacon failed"
  wait_for "stack $k patched beacon API up" 300 'curl -s -m 5 "'"$b"'/eth/v1/node/health" >/dev/null 2>&1'
  for w in $(sk_wallets "$k"); do
    docker run -d --name "vana-vc-$w" --network vana \
      -v "$WORK/vcdata-$w":/vcdata -v "$WORK/$w/prysm":/wallet -v "$WORK/vcpass.txt":/pass:ro \
      -v "$WORK/gen/data/metadata":/cfg:ro "$VC_NEW" \
      --datadir /vcdata --wallet-dir /wallet --wallet-password-file /pass \
      --chain-config-file /cfg/config.yaml --beacon-rpc-provider "$clnm:4000" \
      --suggested-fee-recipient "$FUNDER" --accept-terms-of-use >/dev/null || die "VC $w restart failed"
  done
  wait_for "stack $k peered again" 300 '[ "$(curl -s -m 10 "'"$b"'/eth/v1/node/peer_count" | python3 -c "import sys,json
try: print(json.load(sys.stdin)[\"data\"][\"connected\"])
except Exception: print(0)" 2>/dev/null)" -ge 1 ]'
  wait_for "stack $k caught back up" 600 '[ "$(curl -s -m 10 "'"$b"'/eth/v1/node/syncing" | python3 -c "import sys,json
try: print(json.load(sys.stdin)[\"data\"][\"sync_distance\"])
except Exception: print(9999)" 2>/dev/null)" -le 1 ]'
  ok "stack $k now on the patched build, rejoined and in sync"
}

p6_rolling() {
  c_hdr "PHASE 6  rolling client swap: v5.1.0 -> patched, one stack at a time"
  need_file pipeline/new_contract; need_file pipeline/switch_block
  local k
  for k in $(seq 1 "$STACKS"); do need_running "$(sk_el "$k")" "$(sk_cl "$k")"; done
  local newc swb; newc=$(cat pipeline/new_contract); swb=$(cat pipeline/switch_block)
  local before_slot; before_slot=$(clslot)
  for k in $(seq 1 "$STACKS"); do roll_stack "$k" "$newc" "$swb"; done
  ok "all $STACKS stacks on the patched build - no keys moved, no node retired"
  count_missed_slots "$before_slot" "$(clslot)"
  local base; base=$(clfin)
  wait_for "finality advancing after the roll" 900 '[ "$(clfin)" -gt '"$((base+1))"' ]'
  local pat
  for k in $(seq 1 "$STACKS"); do
    for pat in 'incorrect merkle index' 'Could not process deposit log'; do
      [ "$(docker logs "$(sk_cl "$k")" 2>&1 | grep -ci "$pat")" = "0" ] \
        || die "stack $k deposit log scan failed: $pat"
    done
  done
  ok "deposit-log scan clean on all $STACKS stacks"
}

p6_swap_patched() {
  c_hdr "PHASE 6  live swap to patched Prysm + deposit-contract switch"
  need_running vana-el vana-cl; need_file pipeline/new_contract; need_file pipeline/switch_block
  local new sw pre_slot pre_root
  new=$(cat pipeline/new_contract); sw=$(cat pipeline/switch_block)
  pre_slot=$(clslot)
  pre_root=$(curl -s "$B/eth/v1/beacon/headers/head" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["root"])')
  info "pre-swap head: slot $pre_slot root ${pre_root:0:20}..."
  cp -R cl-data cl-data.v510 && cp -R vcdata vcdata.v510
  ok "databases backed up (the migration is one-way)"
  docker stop vana-vc >/dev/null 2>&1; sleep 3
  docker stop -t 40 vana-cl >/dev/null 2>&1
  docker rm vana-cl vana-vc >/dev/null 2>&1
  ok "v5.1.0 stopped gracefully (geth left running so the heads stay aligned)"
  # --clear-deposit-contract: the DB records the retired address; without this the
  # patched build refuses to start on the mismatch. --sync-from head: resume from
  # the real head instead of the justified checkpoint.
  docker run -d --name vana-cl --network vana \
    -v "$WORK/cl-data":/cldata -v "$WORK/gen/data/metadata":/cfg:ro -v "$WORK/jwtsecret":/jwt:ro \
    -p 3500:3500 -p 4000:4000 -p 8080:8080 "$CL_PATCHED" \
    --datadir /cldata --chain-config-file /cfg/config.yaml --genesis-state /cfg/genesis.ssz \
    --execution-endpoint http://vana-el:8551 --jwt-secret /jwt \
    --contract-deployment-block "$DEPOSIT_DEPLOY_BLOCK" \
    --deposit-contract "$new" --retired-deposit-contract "$RETIRED_DEPOSIT" \
    --deposit-contract-switch-block "$sw" --clear-deposit-contract --sync-from head \
    --accept-terms-of-use \
    --http-host 0.0.0.0 --http-port 3500 --http-cors-domain '*' \
    --rpc-host 0.0.0.0 --rpc-port 4000 --monitoring-host 0.0.0.0 \
    --min-sync-peers 0 --no-discovery >/dev/null || die "patched beacon start failed"
  wait_for "patched beacon API up" 300 'curl -s -m 5 "$B/eth/v1/node/health" >/dev/null 2>&1'
  local post_root; post_root=$(curl -s "$B/eth/v1/beacon/headers/head" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["root"])')
  if [ "$post_root" = "$pre_root" ]; then
    ok "--sync-from head preserved the exact head (slot $(clslot))"
  else
    info "head moved: $pre_slot -> $(clslot); realigning EL to the CL head"
    local pb ph
    pb=$(curl -s "$B/eth/v2/beacon/blocks/head" | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["data"]["message"]["body"]["execution_payload"]["block_number"]))')
    ph=$(curl -s "$B/eth/v2/beacon/blocks/head" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["message"]["body"]["execution_payload"]["block_hash"])')
    rpc debug_setHead "[\"$(python3 -c "print(hex($pb))")\"]" >/dev/null; sleep 8
    [ "$(elhead)" = "$pb" ] || die "EL realignment failed"
    ok "EL realigned to $pb"
  fi
  docker logs "$CL_NAME" 2>&1 | grep -qi 'Cleared the stored deposit contract' && ok "stale deposit-contract record cleared"
  docker run -d --name vana-vc --network vana \
    -v "$WORK/vcdata":/vcdata -v "$WORK/vcwallet/prysm":/wallet -v "$WORK/vcpass.txt":/pass:ro \
    -v "$WORK/gen/data/metadata":/cfg:ro "$VC_NEW" \
    --datadir /vcdata --wallet-dir /wallet --wallet-password-file /pass \
    --chain-config-file /cfg/config.yaml --beacon-rpc-provider vana-cl:4000 \
    --suggested-fee-recipient "$FUNDER" --accept-terms-of-use >/dev/null || die "VC start failed"
  local base; base=$(clfin)
  wait_for "finality advancing past the swap" 900 '[ "$(clfin)" -gt '"$((base+1))"' ]'
  for pat in 'incorrect merkle index' 'Could not process deposit log' 'Unable to process past deposit'; do
    [ "$(docker logs "$CL_NAME" 2>&1 | grep -ci "$pat")" = "0" ] || die "deposit log scan failed: $pat"
  done
  ok "boundary scan clean: 0 merkle-index / deposit-log / past-log errors"
  local spec; spec=$(curl -s "$B/eth/v1/config/spec" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["DEPOSIT_CONTRACT_ADDRESS"])')
  [ "$(echo "$spec" | tr 'A-F' 'a-f')" = "$(echo "$new" | tr 'A-F' 'a-f')" ] || die "beacon reports $spec, expected $new"
  ok "active deposit contract = $new"
}

# ============================== phase 7: Spectra + Fusaka in a single upgrade
# Electra/Prague and Fulu/Osaka are scheduled together: one geth restart carrying
# both fork times, one CL restart carrying both fork epochs. The CL epoch start
# must equal the EL fork time exactly, or the layers fork at different instants.
# Kept for the --stacks 1 path and for resuming an older two-stack run where stack 1 was
# retired. With the rolling design every stack survives, so this is normally a no-op.
retarget_surviving_stack() {
  docker ps --format '{{.Names}}' | grep -qx "$CL_NAME" && return 0
  if docker ps --format '{{.Names}}' | grep -qx "$CL2_NAME"; then
    EL_NAME="$EL2_NAME"; CL_NAME="$CL2_NAME"; R="$R2"; RIN="$RIN2"; B="$B2"
    info "stack 1 is gone; targeting the surviving stack ($EL_NAME / $CL_NAME, $R, $B)"
  fi
}

# Restart ONE stack onto the new fork schedule: geth with --override.genesis, then the
# beacon with the rewritten config, then its validators. Called in a loop so only one
# stack is ever down; the rest carry the chain. --rpc.allow-unprotected-txs is needed on
# every geth because the presigned Nick's-method system-contract txs are pre-EIP-155,
# and --networkid is mutually exclusive with --override.genesis.
roll_stack_forks() {
  local k=$1 new=$2 sw=$3 elnm clnm b elrpc w
  elnm=$(sk_el "$k"); clnm=$(sk_cl "$k"); b=$(sk_beacon "$k"); elrpc=$(sk_rpc "$k")
  info "rolling stack $k onto the fork schedule"
  [ "$STACKS" -gt 1 ] && assert_others_healthy "$k"
  for w in $(sk_wallets "$k"); do
    docker stop -t 20 "vana-vc-$w" >/dev/null 2>&1; docker rm "vana-vc-$w" >/dev/null 2>&1
  done
  docker stop -t 40 "$clnm" >/dev/null 2>&1; docker rm "$clnm" >/dev/null 2>&1
  docker stop -t 40 "$elnm" >/dev/null 2>&1; docker rm "$elnm" >/dev/null 2>&1
  docker run -d --name "$elnm" --network vana \
    -v "$WORK/$(sk_eldata "$k")":/data -v "$WORK/jwtsecret":/jwt:ro \
    -v "$WORK/pipeline/fusaka-genesis.json":/fg.json:ro \
    $(sk_elports "$k") "$GETH_IMG" \
    --datadir /data --nodiscover --maxpeers 25 --ipcdisable --cache 512 \
    --override.genesis /fg.json --rpc.allow-unprotected-txs \
    --http --http.addr 0.0.0.0 --http.port 8545 --http.vhosts '*' \
    --http.api eth,net,web3,debug,txpool,admin \
    --authrpc.addr 0.0.0.0 --authrpc.port 8551 --authrpc.vhosts '*' --authrpc.jwtsecret /jwt \
    --syncmode full --miner.gasprice 1000000000 >/dev/null || die "stack $k geth restart failed"
  wait_for "stack $k geth back up" 300 'curl -s -m 5 -X POST -H "Content-Type: application/json" --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_blockNumber\",\"params\":[],\"id\":1}" '"$elrpc"' >/dev/null 2>&1'
  docker logs "$elnm" 2>&1 | grep -qE 'Head state missing' && die "stack $k geth lost head state on restart"
  docker run -d --name "$clnm" --network vana \
    -v "$WORK/$(sk_cldata "$k")":/cldata -v "$WORK/gen/data/metadata":/cfg:ro -v "$WORK/jwtsecret":/jwt:ro \
    $(sk_clports "$k") "$CL_PATCHED" \
    --datadir /cldata --chain-config-file /cfg/config.yaml --genesis-state /cfg/genesis.ssz \
    --execution-endpoint "http://$elnm:8551" --jwt-secret /jwt \
    --contract-deployment-block "$DEPOSIT_DEPLOY_BLOCK" \
    --deposit-contract "$new" --retired-deposit-contract "$RETIRED_DEPOSIT" \
    --deposit-contract-switch-block "$sw" --sync-from head --accept-terms-of-use \
    --subscribe-all-subnets $(cl_other_peer_args "$k") \
    --http-host 0.0.0.0 --http-port 3500 --http-cors-domain '*' \
    --rpc-host 0.0.0.0 --rpc-port 4000 --monitoring-host 0.0.0.0 \
    --min-sync-peers "$([ "$STACKS" -gt 1 ] && echo 1 || echo 0)" --no-discovery \
    >/dev/null || die "stack $k beacon restart failed"
  wait_for "stack $k beacon back up" 300 'curl -s -m 5 "'"$b"'/eth/v1/node/health" >/dev/null 2>&1'
  for w in $(sk_wallets "$k"); do
    docker run -d --name "vana-vc-$w" --network vana \
      -v "$WORK/vcdata-$w":/vcdata -v "$WORK/$w/prysm":/wallet -v "$WORK/vcpass.txt":/pass:ro \
      -v "$WORK/gen/data/metadata":/cfg:ro "$VC_NEW" \
      --datadir /vcdata --wallet-dir /wallet --wallet-password-file /pass \
      --chain-config-file /cfg/config.yaml --beacon-rpc-provider "$clnm:4000" \
      --suggested-fee-recipient "$FUNDER" --accept-terms-of-use >/dev/null || die "VC $w restart failed"
  done
  if [ "$STACKS" -gt 1 ]; then
    wait_for "stack $k caught back up" 600 '[ "$(curl -s -m 10 "'"$b"'/eth/v1/node/syncing" | python3 -c "import sys,json
try: print(json.load(sys.stdin)[\"data\"][\"sync_distance\"])
except Exception: print(9999)" 2>/dev/null)" -le 1 ]'
  fi
  ok "stack $k rolled onto the fork schedule"
}

p7_fusaka() {
  c_hdr "PHASE 7  upgrade to Spectra then Fusaka"
  retarget_surviving_stack
  need_running "$EL_NAME" "$CL_NAME"; need_file pipeline/new_contract; need_file pipeline/switch_block

  # Idempotency. Re-running this phase must not reschedule forks that are already
  # scheduled or live: moving a fork time the chain has already passed would split
  # the layers. Three cases, in order of how far along the chain is.
  local FAR=18446744073709551615
  local cur_fork sched_e sched_f
  cur_fork=$(clfork)
  sched_e=$(curl -s -m 10 "$B/eth/v1/config/spec" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"].get("ELECTRA_FORK_EPOCH","?"))' 2>/dev/null || echo '?')
  sched_f=$(curl -s -m 10 "$B/eth/v1/config/spec" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"].get("FULU_FORK_EPOCH","?"))' 2>/dev/null || echo '?')
  info "current fork $cur_fork | scheduled: ELECTRA=$sched_e FULU=$sched_f"

  if [ "$cur_fork" = "0x20000095" ]; then
    ok "Fusaka is already live - nothing to do"
    return 0
  fi
  if [ "$sched_e" != "$FAR" ] || [ "$sched_f" != "$FAR" ]; then
    info "forks already scheduled - not rescheduling; ensuring prerequisites and waiting"
    p7a_system_contracts 0
    if [ "$cur_fork" != "0x20000094" ]; then
      wait_for "Electra live (0x20000094)" 3600 '[ "$(clfork)" = "0x20000094" ]'
    fi
    ok "Spectra active"
    wait_for "Fulu live (0x20000095)" 3600 '[ "$(clfork)" = "0x20000095" ]'
    ok "Fusaka active at epoch $(( $(clslot) / 8 ))"
    return 0
  fi
  local gt slot ep e_electra e_fulu prague osaka
  gt=$(grep -E '^MIN_GENESIS_TIME:' gen/data/metadata/config.yaml | awk '{print $2}')
  slot=$(clslot); ep=$((slot/8))
  e_electra=$((ep+14))            # ~11 min: room to deploy the system contracts
  e_fulu=$((e_electra+10))        # ~8 min after Spectra
  prague=$((gt + e_electra*8*6))
  osaka=$((gt + e_fulu*8*6))
  info "head epoch $ep"
  info "ELECTRA epoch $e_electra -> pragueTime $prague"
  info "FULU    epoch $e_fulu -> osakaTime  $osaka"
  python3 -c "
assert $gt + $e_electra*48 == $prague, 'electra/prague misaligned'
assert $gt + $e_fulu*48    == $osaka,  'fulu/osaka misaligned'
print('       fork instants aligned across both layers')" || die "fork alignment check failed"

  # ---- EL genesis override: prague + osaka + blob schedules + EIP-6110 source
  python3 - "$prague" "$osaka" "$(cat pipeline/new_contract)" <<'PY' || die "genesis override build failed"
import sys,json
prague,osaka,dep=int(sys.argv[1]),int(sys.argv[2]),sys.argv[3]
g=json.load(open('genesis.json')); c=g['config']
c['pragueTime']=prague; c['osakaTime']=osaka
bs=c.setdefault('blobSchedule',{})
bs.setdefault('cancun',{"target":3,"max":6,"baseFeeUpdateFraction":3338477})
bs['prague']={"target":6,"max":9,"baseFeeUpdateFraction":5007716}
bs['osaka'] ={"target":6,"max":9,"baseFeeUpdateFraction":5007716}
c['depositContractAddress']=dep          # EIP-6110 reads deposits from here
json.dump(g,open('pipeline/fusaka-genesis.json','w'),indent=1)
PY
  ok "genesis override written (prague+osaka, blobSchedule, EIP-6110 source)"

  # ---- CL config: schedule both forks + declare PeerDAS params explicitly
  python3 - "$e_electra" "$e_fulu" <<'PY' || die "CL config patch failed"
import sys,re
ee,ef=sys.argv[1],sys.argv[2]
for p in ('gen/config/cl/config.yaml','gen/data/metadata/config.yaml'):
    c=open(p).read()
    c=re.sub(r'^ELECTRA_FORK_EPOCH: \d+','ELECTRA_FORK_EPOCH: %s'%ee,c,flags=re.M)
    c=re.sub(r'^FULU_FORK_EPOCH: \d+','FULU_FORK_EPOCH: %s'%ef,c,flags=re.M)
    if 'MAX_EFFECTIVE_BALANCE_ELECTRA' not in c:
        # Prysm's 2048-ETH default sits BELOW Vana's 35,000 MIN_ACTIVATION_BALANCE,
        # which is incoherent; keep the spec's 64x ratio. Replace if Vana publishes one.
        c=re.sub(r'^(MAX_EFFECTIVE_BALANCE: \d+)$',r'\1\nMAX_EFFECTIVE_BALANCE_ELECTRA: 2240000000000000',c,flags=re.M)
    if 'NUMBER_OF_CUSTODY_GROUPS' not in c:
        c=c.rstrip()+"""
SAMPLES_PER_SLOT: 8
NUMBER_OF_CUSTODY_GROUPS: 128
CUSTODY_REQUIREMENT: 4
VALIDATOR_CUSTODY_REQUIREMENT: 8
DATA_COLUMN_SIDECAR_SUBNET_COUNT: 128
MAX_REQUEST_DATA_COLUMN_SIDECARS: 16384
BLOB_SCHEDULE:
  - EPOCH: 0
    MAX_BLOBS_PER_BLOCK: 6
  - EPOCH: %s
    MAX_BLOBS_PER_BLOCK: 9
  - EPOCH: %s
    MAX_BLOBS_PER_BLOCK: 9
"""%(ee,ef)
    open(p,'w').write(c)
PY
  ok "CL config: ELECTRA=$e_electra FULU=$e_fulu, PeerDAS params + BLOB_SCHEDULE declared"

  # ---- roll every stack onto the new fork schedule, ONE AT A TIME so the chain keeps
  #      producing throughout. The genesis override and the CL config above are shared
  #      files, so each stack picks them up when its own containers come back.
  local new sw k; new=$(cat pipeline/new_contract); sw=$(cat pipeline/switch_block)
  for k in $(seq 1 "$STACKS"); do roll_stack_forks "$k" "$new" "$sw"; done
  ok "all $STACKS stacks restarted with both forks scheduled"

  # ---- deploy the Prague system contracts BEFORE pragueTime
  p7a_system_contracts "$prague"

  wait_for "Electra live (fork 0x20000094)" 1800 '[ "$(clfork)" = "0x20000094" ]'
  ok "Spectra active at epoch $(( $(clslot) / 8 ))"
  local rh; rh=$(rpcv eth_getBlockByNumber '["latest",false]' >/dev/null; curl -s -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}' "$R" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"].get("requestsHash") or "none")')
  [ "$rh" != "none" ] || die "Prague active on the CL but blocks carry no requestsHash"
  ok "requestsHash present on EL blocks"
  wait_for "Fulu live (fork 0x20000095)" 1800 '[ "$(clfork)" = "0x20000095" ]'
  ok "Fusaka active at epoch $(( $(clslot) / 8 ))"
}

# deploy EIP-2935 / 7002 / 7251 from their canonical presigned transactions
p7a_system_contracts() {
  retarget_surviving_stack
  local prague="$1"
  c_hdr "PHASE 7a  Prague system contracts (presigned, must land before pragueTime)"
  if [ "$prague" = "0" ]; then
    prague=$(curl -s -m 10 -X POST -H 'Content-Type: application/json' \
      --data '{"jsonrpc":"2.0","method":"admin_nodeInfo","params":[],"id":1}' "$R" \
      | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["protocols"]["eth"]["config"].get("pragueTime",0))' 2>/dev/null || echo 0)
    info "pragueTime read from the running node: $prague"
  fi
  wait_for "chain producing blocks again" 600 '[ "$(clslot)" -gt 0 ]'
  # The three canonical presigned ("Nick's method") deployment transactions are
  # frozen in system-contracts.txt: eip, address, sender, gas cost, raw tx, expected
  # hash. Regenerate with ./gen-system-contracts.sh (curl-based). They are NOT fetched
  # at run time - python's urllib has no CA bundle on a python.org install, and a
  # pipeline that reaches out to GitHub mid-fork is a needless failure mode.
  need_file "$WORK/system-contracts.txt"
  cp "$WORK/system-contracts.txt" pipeline/sysc.txt
  while read -r eip addr sender cost raw want; do
    local got
    got=$(docker run --rm --entrypoint sh "$FOUNDRY" -c "cast keccak $raw" 2>/dev/null | tr -d '\r\n')
    [ "$got" = "$want" ] || die "EIP-$eip tx hash $got != canonical $want"
    info "EIP-$eip verified against the canonical hash"
    local have; have=$(rpcv eth_getCode "[\"$addr\",\"latest\"]")
    if [ -n "$have" ] && [ "$have" != "0x" ]; then
      info "EIP-$eip already deployed at $addr - skipping (no funding needed)"
      continue
    fi
    local bal; bal=$(python3 -c "print(int('$(rpcv eth_getBalance "[\"$sender\",\"latest\"]")',16))")
    if [ "$bal" -lt "$cost" ]; then
      cast_in "cast send --private-key $FUNDER_KEY --rpc-url $RIN --value 0.3ether \
               --gas-price 3000000000 --priority-gas-price 2000000000 $sender" >/dev/null
      wait_for "deployer $eip funded" 240 '[ "$(python3 -c "print(int('"'"'$(rpcv eth_getBalance "[\"'"$sender"'\",\"latest\"]")'"'"',16))")" -ge '"$cost"' ]'
    fi
    rpc eth_sendRawTransaction "[\"$raw\"]" | grep -q '"result"' || die "EIP-$eip broadcast rejected"
  done < pipeline/sysc.txt
  while read -r eip addr sender cost raw want; do
    wait_for "EIP-$eip deployed at $addr" 300 '[ -n "$(rpcv eth_getCode "[\"'"$addr"'\",\"latest\"]")" ] && [ "$(rpcv eth_getCode "[\"'"$addr"'\",\"latest\"]")" != "0x" ]'
  done < pipeline/sysc.txt
  local now; now=$(date +%s)
  if [ "${prague:-0}" -gt 0 ] && [ "$now" -ge "$prague" ]; then
    info "note: pragueTime already passed; contracts are present so the fork can proceed"
  fi
  if [ "${prague:-0}" -gt 0 ] && [ "$now" -lt "$prague" ]; then
    ok "all three deployed with $(( (prague-now)/60 )) min to spare before pragueTime"
  else
    ok "all three system contracts present"
  fi
}

# ================================================================ phase 8: report
p8_report() {
  retarget_surviving_stack
  c_hdr "PHASE 8  final verification"
  local slot fin fork el
  slot=$(clslot); fin=$(clfin); fork=$(clfork); el=$(elhead)
  printf '  %-28s %s\n' "CL fork version" "$fork"
  printf '  %-28s %s (epoch %s)\n' "head slot" "$slot" "$((slot/8))"
  printf '  %-28s %s\n' "finalized epoch" "$fin"
  printf '  %-28s %s\n' "EL head" "$el"
  printf '  %-28s %s\n' "deposit contract" "$(cat pipeline/new_contract)"
  printf '  %-28s %s\n' "switch block" "$(cat pipeline/switch_block)"
  curl -s "$B/eth/v1/beacon/states/head/validators" | python3 -c '
import sys,json
from collections import Counter
d=json.load(sys.stdin)["data"]
print("  %-28s %d %s"%("validators",len(d),dict(Counter(v["status"] for v in d))))'
  for pat in 'incorrect merkle index' 'Could not process deposit log' 'invalid payload timestamp'; do
    printf '  %-28s %s\n' "$pat" "$(docker logs "$CL_NAME" 2>&1 | grep -ci "$pat")"
  done
  printf '  %-28s %s\n' "Ignoring beacon update" "$(docker logs "$EL_NAME" 2>&1 | grep -c 'Ignoring beacon update')"
  [ "$fork" = "0x20000095" ] || die "expected Fulu 0x20000095, got $fork"
  [ "$fin" -gt 0 ] || die "chain is not finalizing"
  c_hdr "PIPELINE COMPLETE - Fusaka live, chain continuous from the Deneb genesis"
}

# ======================================================================= driver
c_hdr "Vana network pipeline  (mode=$MODE, stop-after=$STOP_AFTER)"
info "this destroys any existing vana-* network and its consensus history"
if [ "$START_AT" -le 1 ]; then
  confirm "proceed?"
  p0_teardown
else
  info "resuming at phase $START_AT - skipping teardown, keeping the running network"
fi
phase_gate 1 &&        p1_chaindata
phase_gate 2 &&        p2_el_align
phase_gate 3 &&        p3_genesis
phase_gate 4 &&        p4_start_v51
phase_gate 5 &&        p5_deposit_contract
phase_gate 6 &&        { [ "$STACKS" -gt 1 ] && p6_rolling || p6_swap_patched; }
phase_gate 7 &&        p7_fusaka
phase_gate 8 &&        p8_report
