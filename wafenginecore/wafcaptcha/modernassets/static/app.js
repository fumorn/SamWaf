/* SamWaf modern Cap bridge in the original challenge-page style.
 * Same language switch, toast and success flow as the legacy page; the v2
 * endpoints and resource wiring live underneath. No CDN, no token storage,
 * no request replay: navigation uses location.replace (fresh GET), never reload. */
(() => {
  'use strict';
  const page = document.getElementById('samwaf-cap-page');
  const prefix = page.dataset.prefix;
  const api = `${prefix}/v2/`;
  const assets = `${prefix}/v2/assets/`;
  const originalGET = page.dataset.requestMethod === 'GET';
  const widget = document.getElementById('cap');
  const langSwitch = document.getElementById('lang-switch');
  const container = document.querySelector('.container');

  // 语言资源（与原版页面一致；标题与说明来自站点配置）
  const i18n = {
    'zh': {
      title: '进行验证',
      verifySuccess: '验证成功，页面即将刷新...',
      verifyFail: '验证失败',
      validationError: '验证异常',
      switchLang: 'English',
      infoTitle: page.dataset.titleZh || '安全验证',
      infoText: page.dataset.textZh || '为了确保您的访问安全，请完成以下验证',
      capWidget: {
        verifyingLabel: '验证中...',
        initialState: '我是人类',
        solvedLabel: '我是人类',
        errorLabel: '错误'
      }
    },
    'en': {
      title: 'Verification',
      verifySuccess: 'Verification successful, page will refresh...',
      verifyFail: 'Verification failed',
      validationError: 'Validation error',
      switchLang: '中文',
      infoTitle: page.dataset.titleEn || 'Security Verification',
      infoText: page.dataset.textEn || 'To ensure the security of your access, please complete the following verification',
      capWidget: {
        verifyingLabel: 'Verifying...',
        initialState: "I'm a human",
        solvedLabel: "I'm a human",
        errorLabel: 'Error'
      }
    }
  };

  // 语言管理（与原版页面一致：Cookie 记忆 + 浏览器语言探测）
  const langManager = (function() {
    function setCookie(name, value, days) {
      let expires = "";
      if (days) {
        const date = new Date();
        date.setTime(date.getTime() + (days * 24 * 60 * 60 * 1000));
        expires = "; expires=" + date.toUTCString();
      }
      document.cookie = name + "=" + (value || "") + expires + "; path=/";
    }

    function getCookie(name) {
      const nameEQ = name + "=";
      const ca = document.cookie.split(';');
      for (let i = 0; i < ca.length; i++) {
        let c = ca[i];
        while (c.charAt(0) === ' ') c = c.substring(1, c.length);
        if (c.indexOf(nameEQ) === 0) return c.substring(nameEQ.length, c.length);
      }
      return null;
    }

    function detectBrowserLang() {
      const browserLang = navigator.language || navigator.userLanguage;
      return browserLang.toLowerCase().startsWith('zh') ? 'zh' : 'en';
    }

    let currentLang = getCookie('samwaf_lang') || detectBrowserLang();

    function setLanguage(lang) {
      currentLang = lang;
      setCookie('samwaf_lang', lang, 30);
      applyLanguage();
      // 与原版一致：切换语言后重新加载验证页。location.replace 走全新 GET，
      // 不会像 reload 那样重放被拦截的 POST 请求体。
      window.location.replace(window.location.href);
      return currentLang;
    }

    function getCurrentLang() {
      return currentLang;
    }

    function getText(key) {
      return i18n[currentLang][key] || i18n['en'][key] || key;
    }

    function getCapWidgetText(key) {
      return i18n[currentLang].capWidget[key] || i18n['en'].capWidget[key] || key;
    }

    function applyLanguage() {
      document.getElementById('page-title').textContent = getText('title');
      document.getElementById('lang-switch').textContent = getText('switchLang');

      const infoTitle = document.getElementById('info-title');
      const infoText = document.getElementById('info-text');
      if (infoTitle) {
        infoTitle.textContent = getText('infoTitle');
      }
      if (infoText) {
        infoText.textContent = getText('infoText');
      }

      const capWidget = document.getElementById('cap');
      if (capWidget) {
        capWidget.setAttribute('data-cap-i18n-verifying-label', getCapWidgetText('verifyingLabel'));
        capWidget.setAttribute('data-cap-i18n-initial-state', getCapWidgetText('initialState'));
        capWidget.setAttribute('data-cap-i18n-solved-label', getCapWidgetText('solvedLabel'));
        capWidget.setAttribute('data-cap-i18n-error-label', getCapWidgetText('errorLabel'));
      }
    }

    return {
      setLanguage,
      getCurrentLang,
      getText,
      getCapWidgetText,
      applyLanguage
    };
  })();

  function showToast(message, type = 'success') {
    const existingToast = document.querySelector('.toast');
    if (existingToast) {
      existingToast.remove();
    }

    const toast = document.createElement('div');
    toast.className = `toast ${type}`;
    toast.textContent = message;
    document.body.appendChild(toast);

    setTimeout(() => {
      toast.classList.add('show');
    }, 100);

    setTimeout(() => {
      toast.classList.remove('show');
      setTimeout(() => {
        if (toast.parentNode) {
          toast.parentNode.removeChild(toast);
        }
      }, 300);
    }, 2000);
  }

  // 验证完成后向后台提交 token 换取放行凭证（与原版流程一致，端点为 v2）
  let validating = false;
  let completed = false;

  widget.addEventListener("solve", function (e) {
    const token = e.detail.token;
    validateToken(token);
  });

  widget.addEventListener("error", function () {
    showToast(langManager.getText('verifyFail'), 'error');
  });

  async function validateToken(token) {
    if (validating || completed) return;
    if (typeof token !== 'string' || !token) { showToast(langManager.getText('validationError'), 'error'); return; }
    validating = true;
    try {
      const response = await fetch(`${api}validate`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ token: token }),
        credentials: 'same-origin',
        cache: 'no-store',
        redirect: 'error'
      });
      if (!response.ok) {
        throw new Error(`HTTP error! status: ${response.status}`);
      }
      const result = await response.json();

      if (result.success !== true) {
        showToast(langManager.getText('verifyFail'), 'error');
        return;
      }
      completed = true;
      // 隐藏验证码元素与语言切换按钮
      document.querySelector('#cap').style.display = 'none';
      document.getElementById('lang-switch').style.display = 'none';

      const successEl = document.createElement('div');
      successEl.className = 'success-message';
      successEl.textContent = langManager.getText('verifySuccess');
      document.querySelector('.container').appendChild(successEl);

      showToast(langManager.getText('verifySuccess'), 'success');

      // 原版为 2 秒后 reload；这里用 location.replace（全新 GET）避免重放
      // 被拦截的非 GET 请求体。非 GET 不自动跳转，由访客自行返回重试。
      if (originalGET) {
        setTimeout(() => {
          window.location.replace(window.location.href);
        }, 2000);
      }
    } catch (error) {
      showToast(langManager.getText('validationError'), 'error');
    } finally {
      validating = false;
    }
  }

  // 语言切换按钮（与原版一致）
  document.getElementById('lang-switch').addEventListener('click', function () {
    const newLang = langManager.getCurrentLang() === 'zh' ? 'en' : 'zh';
    langManager.setLanguage(newLang);
  });

  // Set all upstream resource overrides BEFORE loading Cap (it preloads SHA WASM).
  window.CAP_CUSTOM_WASM_URL = `${assets}cap_wasm_bg.wasm`;
  window.CAP_CUSTOM_HASHWX_URL = `${assets}hashwx.wasm`;
  window.CAP_PAKO_URL = `${assets}pako_inflate.min.js`;
  window.CAP_DISABLE_WIDGET_REF = true;
  window.CAP_DISABLE_HAPTICS = true;
  // Cap uses this for challenge/redeem only. Reject unexpected endpoints/redirects.
  window.CAP_CUSTOM_FETCH = async (url, options = {}) => {
    const target = new URL(url, location.href);
    if (target.origin !== location.origin || ![`${api}challenge`, `${api}redeem`].includes(target.pathname) || target.search || target.hash) {
      throw new Error('Unexpected verification endpoint');
    }
    const controller = new AbortController();
    const abort = () => controller.abort();
    if (options.signal?.aborted) controller.abort();
    options.signal?.addEventListener('abort', abort, { once: true });
    const timer = setTimeout(abort, 30000);
    try {
      const response = await fetch(target.href, { ...options, signal: controller.signal, credentials: 'same-origin', cache: 'no-store', redirect: 'error' });
      if (!response.ok) throw new Error('Verification HTTP error');
      const body = await response.arrayBuffer();
      return new Response(body, { status: response.status, headers: response.headers });
    } finally {
      clearTimeout(timer);
      options.signal?.removeEventListener('abort', abort);
    }
  };

  function loadLibrary() {
    if (customElements.get('cap-widget')) return Promise.resolve();
    const script = document.createElement('script');
    script.src = `${assets}cap.js`;
    document.head.appendChild(script);
    return new Promise((resolve, reject) => {
      script.onload = () => (customElements.get('cap-widget') ? resolve() : reject(new Error('Cap unavailable')));
      script.onerror = () => reject(new Error('load failed'));
    });
  }

  widget.setAttribute('data-cap-worker-count', String(Math.min(4, navigator.hardwareConcurrency || 2)));
  langManager.applyLanguage();
  loadLibrary().catch(() => showToast(langManager.getText('validationError'), 'error'));
})();
