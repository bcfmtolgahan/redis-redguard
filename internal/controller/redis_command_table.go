/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Command and category metadata derived from the COMMAND table of
// redis:7-alpine (Redis 7.4). Regenerate against a new base image when the
// pinned Redis version changes.

// redisAllowedCommands is the set of commands and subcommands a RedisUser may
// grant individually with +cmd. Membership is derived from Redis: a command
// qualifies only if it carries a key specification, so a ~pattern confines it,
// or it touches neither keys nor channels (connection, transaction and pub/sub
// messaging). Commands that operate server-wide or across the whole keyspace
// (KEYS, SCAN, FLUSHALL, MIGRATE, MONITOR, PSYNC, CONFIG, ...) are absent and
// therefore rejected. A denylist would default every future Redis command to
// permitted; this allowlist defaults them to rejected.
var redisAllowedCommands = map[string]struct{}{
	"append": {}, "auth": {}, "bitcount": {}, "bitfield": {},
	"bitfield_ro": {}, "bitop": {}, "bitpos": {}, "blmove": {},
	"blmpop": {}, "blpop": {}, "brpop": {}, "brpoplpush": {},
	"bzmpop": {}, "bzpopmax": {}, "bzpopmin": {}, "client|getname": {},
	"client|id": {}, "client|info": {}, "client|no-touch": {}, "client|reply": {},
	"client|setinfo": {}, "client|setname": {}, "command": {}, "command|count": {},
	"command|docs": {}, "command|getkeys": {}, "command|getkeysandflags": {}, "command|help": {},
	"command|info": {}, "command|list": {}, "copy": {}, "decr": {},
	"decrby": {}, "del": {}, "discard": {}, "dump": {},
	"echo": {}, "eval": {}, "eval_ro": {}, "evalsha": {},
	"evalsha_ro": {}, "exec": {}, "exists": {}, "expire": {},
	"expireat": {}, "expiretime": {}, "fcall": {}, "fcall_ro": {},
	"geoadd": {}, "geodist": {}, "geohash": {}, "geopos": {},
	"georadius": {}, "georadius_ro": {}, "georadiusbymember": {}, "georadiusbymember_ro": {},
	"geosearch": {}, "geosearchstore": {}, "get": {}, "getbit": {},
	"getdel": {}, "getex": {}, "getrange": {}, "getset": {},
	"hdel": {}, "hello": {}, "hexists": {}, "hexpire": {},
	"hexpireat": {}, "hexpiretime": {}, "hget": {}, "hgetall": {},
	"hincrby": {}, "hincrbyfloat": {}, "hkeys": {}, "hlen": {},
	"hmget": {}, "hmset": {}, "hpersist": {}, "hpexpire": {},
	"hpexpireat": {}, "hpexpiretime": {}, "hpttl": {}, "hrandfield": {},
	"hscan": {}, "hset": {}, "hsetnx": {}, "hstrlen": {},
	"httl": {}, "hvals": {}, "incr": {}, "incrby": {},
	"incrbyfloat": {}, "lcs": {}, "lindex": {}, "linsert": {},
	"llen": {}, "lmove": {}, "lmpop": {}, "lpop": {},
	"lpos": {}, "lpush": {}, "lpushx": {}, "lrange": {},
	"lrem": {}, "lset": {}, "ltrim": {}, "memory|usage": {},
	"mget": {}, "move": {}, "mset": {}, "msetnx": {},
	"multi": {}, "object|encoding": {}, "object|freq": {}, "object|idletime": {},
	"object|refcount": {}, "persist": {}, "pexpire": {}, "pexpireat": {},
	"pexpiretime": {}, "pfadd": {}, "pfcount": {}, "pfmerge": {},
	"ping": {}, "psetex": {}, "psubscribe": {}, "pttl": {},
	"publish": {}, "punsubscribe": {}, "quit": {}, "rename": {},
	"renamenx": {}, "reset": {}, "rpop": {}, "rpoplpush": {},
	"rpush": {}, "rpushx": {}, "sadd": {}, "scard": {},
	"sdiff": {}, "sdiffstore": {}, "select": {}, "set": {},
	"setbit": {}, "setex": {}, "setnx": {}, "setrange": {},
	"sinter": {}, "sintercard": {}, "sinterstore": {}, "sismember": {},
	"smembers": {}, "smismember": {}, "smove": {}, "spop": {},
	"spublish": {}, "srandmember": {}, "srem": {}, "sscan": {},
	"ssubscribe": {}, "strlen": {}, "subscribe": {}, "substr": {},
	"sunion": {}, "sunionstore": {}, "sunsubscribe": {}, "touch": {},
	"ttl": {}, "type": {}, "unlink": {}, "unsubscribe": {},
	"unwatch": {}, "watch": {}, "xack": {}, "xadd": {},
	"xautoclaim": {}, "xclaim": {}, "xdel": {}, "xgroup|create": {},
	"xgroup|createconsumer": {}, "xgroup|delconsumer": {}, "xgroup|destroy": {}, "xgroup|setid": {},
	"xinfo|consumers": {}, "xinfo|groups": {}, "xinfo|stream": {}, "xlen": {},
	"xpending": {}, "xrange": {}, "xread": {}, "xreadgroup": {},
	"xrevrange": {}, "xsetid": {}, "xtrim": {}, "zadd": {},
	"zcard": {}, "zcount": {}, "zdiff": {}, "zdiffstore": {},
	"zincrby": {}, "zinter": {}, "zintercard": {}, "zinterstore": {},
	"zlexcount": {}, "zmpop": {}, "zmscore": {}, "zpopmax": {},
	"zpopmin": {}, "zrandmember": {}, "zrange": {}, "zrangebylex": {},
	"zrangebyscore": {}, "zrangestore": {}, "zrank": {}, "zrem": {},
	"zremrangebylex": {}, "zremrangebyrank": {}, "zremrangebyscore": {}, "zrevrange": {},
	"zrevrangebylex": {}, "zrevrangebyscore": {}, "zrevrank": {}, "zscan": {},
	"zscore": {}, "zunion": {}, "zunionstore": {},
}

// redisACLCategories is the set of ACL categories Redis 7.4 defines, minus the
// unscoped families all, admin and dangerous. A +@category grant is accepted
// only for a name in this set; the confinement floor then strips the
// keyspace-wide and administrative commands any category bundles.
var redisACLCategories = map[string]struct{}{
	"bitmap": {}, "blocking": {}, "connection": {}, "fast": {},
	"geo": {}, "hash": {}, "hyperloglog": {}, "keyspace": {},
	"list": {}, "pubsub": {}, "read": {}, "scripting": {},
	"set": {}, "slow": {}, "sortedset": {}, "stream": {},
	"string": {}, "transaction": {}, "write": {},
}
