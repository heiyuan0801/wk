// 腾讯验证码（TCaptcha）加载与调用。
//
// 只在 OneID 判定号码需要人机校验时才会用到。SDK 是腾讯托管的全局脚本，
// 挂 window.TencentCaptcha，用法为 `new TencentCaptcha(appId, callback)` + `show()`，
// 回调收到 { ret, ticket, randstr }（注意 SDK 用小写 randstr，而回灌上游要用
// 大写的 randStr，转换点就在这里）。
//
// 为什么不把 SDK 静态引到 index.html：绝大多数登录根本不需要它，
// 动态注入可以让没触发校验的用户完全不下载这 100KB+ 的脚本。
// 也正因为是动态注入，必须自己处理"重复加载"和"加载失败"。

const SCRIPT_SRC = 'https://turing.captcha.qcloud.com/TCaptcha.js';

// loadPromise 缓存加载中的 Promise，避免并发调用注入多个 <script>。
// 腾讯 SDK 内部有 __TencentCaptchaExists__ 守卫，重复引用会直接抛错。
let loadPromise = null;

// loadTencentCaptcha 注入并等待 SDK 就绪，返回构造函数。
export function loadTencentCaptcha() {
  if (typeof window !== 'undefined' && typeof window.TencentCaptcha === 'function') {
    return Promise.resolve(window.TencentCaptcha);
  }
  if (loadPromise) return loadPromise;

  loadPromise = new Promise((resolve, reject) => {
    const existing = document.querySelector(`script[src="${SCRIPT_SRC}"]`);
    const done = () => {
      if (typeof window.TencentCaptcha === 'function') resolve(window.TencentCaptcha);
      else reject(new Error('验证码组件加载异常'));
    };
    const fail = () => {
      // 失败后清空缓存，下次可以重试（网络抖动很常见）。
      loadPromise = null;
      reject(new Error('验证码组件加载失败，请检查网络后重试'));
    };

    if (existing) {
      existing.addEventListener('load', done);
      existing.addEventListener('error', fail);
      return;
    }
    const script = document.createElement('script');
    script.src = SCRIPT_SRC;
    script.async = true;
    script.addEventListener('load', done);
    script.addEventListener('error', fail);
    document.head.appendChild(script);
  });
  return loadPromise;
}

// runTencentCaptcha 弹出验证码并等待用户完成，解析为 { ticket, randStr, cloudType }。
//
// cloudType 取自调用方给的方案，因为回灌上游时它必须与所用 appId 那条一致；
// SDK 回调里不带这个信息。
export async function runTencentCaptcha(appId, cloudType) {
  if (!appId) throw new Error('缺少验证码 appId');
  const TencentCaptcha = await loadTencentCaptcha();

  return new Promise((resolve, reject) => {
    let settled = false;
    let instance = null;
    const finish = (fn, value) => {
      if (settled) return;
      settled = true;
      try { instance?.destroy?.(); } catch { /* 销毁失败不影响结果 */ }
      fn(value);
    };

    try {
      instance = new TencentCaptcha(appId, res => {
        // 关键：失败时腾讯仍然回调 ret=0，并给出一张 trerror_* 的"错误票据"
        // （例如 appId 不被当前域名允许时是 errorCode 1006）。若只看 ret 和
        // ticket 是否存在，就会把这种垃圾票据当成成功提交给上游，
        // 用户只会看到一句莫名其妙的失败。必须先剔除错误票据。
        const bogus = !res
          || String(res.ticket || '').startsWith('trerror_')
          || Number(res.errorCode) > 0
          || !!res.errorMessage;
        if (bogus) {
          const code = res?.errorCode ? `（错误码 ${res.errorCode}）` : '';
          finish(reject, Object.assign(
            new Error(`人机校验无法启动${code}，请尝试其他验证方式`),
            { code: Number(res?.errorCode) || 0, appId },
          ));
          return;
        }
        if (res.ret === 0 && res.ticket && res.randstr) {
          finish(resolve, { ticket: res.ticket, randStr: res.randstr, cloudType });
        } else if (res.ret === 2) {
          // 用户主动关闭弹窗，属于取消而非错误。
          finish(reject, Object.assign(new Error('已取消人机校验'), { cancelled: true }));
        } else {
          finish(reject, new Error(`人机校验未通过（ret=${res.ret}）`));
        }
      });
      instance.show();
    } catch (error) {
      finish(reject, error instanceof Error ? error : new Error(String(error)));
    }
  });
}

// 仅供测试重置模块级缓存。
export function __resetCaptchaLoader() {
  loadPromise = null;
}

// orderCaptchaOptions 把已知可用的方案排到前面。
//
// 实测：从非 codebuddy.cn 域名发起时，teg 那条的 cap_union_prehandle 会被
// 腾讯以 403 拒绝（SDK 回调带 trerror_* 错误票据），而 tencent 正常渲染。
// 上游给的顺序恰好是 teg 在前，照着渲染会让用户先点一个注定失败的按钮。
// 不直接过滤掉 teg：万一部署在受信域名上它可能可用，留着作为后备。
export function orderCaptchaOptions(options) {
  const list = Array.isArray(options) ? options.slice() : [];
  return list.sort((a, b) => {
    const rank = o => (o?.cloudType === 'tencent' ? 0 : 1);
    return rank(a) - rank(b);
  });
}
