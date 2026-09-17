// GA4 的初始化片段。
//
// 单独一个文件而不是内联 <script>：内联脚本需要 CSP 里的 'unsafe-inline'
// 或者逐页算 nonce，前者等于把 script-src 整个作废，后者会让页面不可缓存。
// 外部文件走 'self'，CSP 那条 script-src 一个字都不用改。
(function () {
  var el = document.currentScript;
  var id = el && el.dataset.ga4;
  if (!id) return;
  window.dataLayer = window.dataLayer || [];
  function gtag() { window.dataLayer.push(arguments); }
  window.gtag = gtag;
  gtag('js', new Date());
  gtag('config', id);
})();
