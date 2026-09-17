// site.js —— 只在公开页面加载。
//
// 公开页几乎不需要 JS，这是这个站的论点之一，所以这个文件应该一直很短。
// 它要是开始变长，说明有东西跑错了层。
(() => {
  'use strict';

  // 6. 大纲：高亮当前读到的那一节
  //
  // 纯增强。目录本身是服务端渲染的，没有 JS 一样能点、能跳；这里只负责
  // 把"你现在读到哪"标出来。
  (() => {
    const toc = document.querySelector('.toc');
    if (!toc) return;
    const links = [...toc.querySelectorAll('a[href^="#"]')];
    if (!links.length) return;

    // decodeURIComponent：锚点是中文，href 里是百分号编码的形式
    const targets = links
      .map((a) => {
        let id;
        try { id = decodeURIComponent(a.getAttribute('href').slice(1)); }
        catch (_) { id = a.getAttribute('href').slice(1); }
        return { a, el: document.getElementById(id) };
      })
      .filter((t) => t.el);
    if (!targets.length) return;

    const mark = (a) => {
      for (const t of targets) t.a.toggleAttribute('data-current', t.a === a);
    };

    if (!('IntersectionObserver' in window)) return;

    // 记录每个标题当前是否在视口上方或之内，再挑"最后一个已经划过顶部的"。
    // 只看 isIntersecting 的话，两个标题同屏时会来回跳。
    const seen = new Map();
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) seen.set(e.target, e);
        let active = null;
        for (const t of targets) {
          const e = seen.get(t.el);
          if (e && e.boundingClientRect.top < 120) active = t.a;
        }
        mark(active || targets[0].a);
      },
      // 顶部留出 sticky 导航的高度，底部收窄让"当前节"更贴近阅读位置
      { rootMargin: '-76px 0px -70% 0px', threshold: 0 }
    );
    for (const t of targets) io.observe(t.el);
  })();
})();
