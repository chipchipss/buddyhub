/* ══════════════════════════════════════════════════════════════════
   loginflow.js · 跨重渲染存活的登录/授权会话
   ══════════════════════════════════════════════════════════════════

   面板视图每 5 秒 tick 重渲染一次，用户在浏览器里完成授权往往要几十秒——
   期间模块状态若只挂在 DOM 闭包里，一次重渲染或一次页面刷新就丢，表现为
   「授权完成了却一直没入池 / 一直停在等待授权」。此前 ext-add、zai 各自
   实现了一遍「模块级状态 + localStorage 持久化」，这里是抽出来的通用件。

   提供两样：
     1. flowStore(name)   —— localStorage 会话袋（按 TTL 自动过期清除）
     2. pollLoop()        —— 可注册多路轮询的循环器（面板重建不断线，
                              终态/超时自动收尾）

   会话数据形状由调用方定义（例如 { session, d, at }），本模块只管存取与过期。
   ══════════════════════════════════════════════════════════════════ */

const NS = 'buddyhub.flow.';

/** flowStore(name) —— 一个带 TTL 的 localStorage 键位。 */
export function flowStore(name) {
  const key = NS + name;
  const read = () => {
    try {
      const raw = localStorage.getItem(key);
      if (!raw) return null;
      const v = JSON.parse(raw);
      return v;
    } catch { return null; }
  };
  return {
    get() { return read(); },
    set(v) { try { localStorage.setItem(key, JSON.stringify(v)); } catch { /* 私密模式 */ } },
    clear() { try { localStorage.removeItem(key); } catch { /* 私密模式 */ } },
    /** getFresh(ttlMs) —— 取回未过期的会话；过期即清除并返回 null。 */
    getFresh(ttlMs) {
      const v = read();
      if (!v) return null;
      if (!v.at || Date.now() - v.at > ttlMs) { this.clear(); return null; }
      return v;
    },
  };
}

/**
 * pollLoop() —— 轮询循环器。
 *   register(job) → 启动一个 job { every, run(), done() }：
 *     every  间隔 ms
 *     run()  每轮调用；返回 true = 本轮有进展
 *     done() 收尾（终态/超时后由调用方显式调用）
 *   同一时刻一个循环器只跑一个 job；新 register 顶掉旧的（先停旧定时器，
 *   不调旧 job 的 done——那是新会话接管的语义，与 ext-add 的既有行为一致）。
 *   stop() 停当前定时器但不动会话数据（被顶掉时用）；
 *   finish() 停定时器并清空当前 job。
 */
export function pollLoop() {
  let timer = null;
  let job = null;
  const clear = () => { if (timer) { clearInterval(timer); timer = null; } };
  return {
    get active() { return !!job; },
    register(next) {
      clear();
      job = next;
      timer = setInterval(() => {
        Promise.resolve().then(() => job && job.run()).catch(() => {});
      }, Math.max(500, next.every || 2000));
      // 首轮立即执行，不等第一个间隔（与既有登录轮询行为一致）
      Promise.resolve().then(() => job && job.run()).catch(() => {});
    },
    stop() { clear(); },
    finish() { clear(); job = null; },
  };
}
