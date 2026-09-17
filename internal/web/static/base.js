// base.js —— 公开页和后台共用的那一层。
//
// 渐进增强，不是前端框架：所有表单在关掉 JS 的情况下都能正常提交。
// 这里只放不属于任何一个页面的通用件——危险操作确认、提示框、下拉菜单、
// 弹窗、站标、一键复制。
//
// 这一层是 site.js 和 admin.js 之间的契约，往里加东西之前先确认两边都要。
(() => {
  'use strict';

  // 1. 删除/吊销这类不可逆操作的二次确认
  for (const f of document.querySelectorAll('form[data-confirm]')) {
    f.addEventListener('submit', e => {
      if (!confirm(f.dataset.confirm)) e.preventDefault();
    });
  }


  // 3. 提示框
  //
  // 全站共用一个挂在 <body> 下的元素，任何带 data-tip 的东西都能触发。
  //
  // 为什么不用 ::after 画在触发元素上：表格外面套着 overflow-x: auto，
  // 伪元素会被那一层裁掉。position: fixed 的独立元素不受祖先裁剪影响。
  //
  // 为什么坐标走 CSS 自定义属性：站点 CSP 是 style-src 'self'，
  // el.setAttribute('style', ...) 会被拦掉，而 el.style.setProperty() 不会。
  // 所以真正的样式留在样式表里，这里只喂两个数字。
  (() => {
    let tip = null;
    let current = null;

    const ensure = () => {
      if (tip) return tip;
      tip = document.createElement('div');
      tip.className = 'tip';
      // 提示文字和触发元素的 aria-label 是同一句话。让读屏再念一遍
      // 只会变成重复播报，所以这个元素对辅助技术完全隐身。
      tip.setAttribute('aria-hidden', 'true');
      document.body.appendChild(tip);
      return tip;
    };

    const show = (el) => {
      const text = el.dataset.tip;
      if (!text) return;
      current = el;
      const t = ensure();
      t.textContent = text;
      const r = el.getBoundingClientRect();
      // 贴着视口顶部时翻到下方，否则提示框会被截掉
      const below = r.top < 40;
      t.dataset.place = below ? 'below' : 'above';
      t.style.setProperty('--tip-x', Math.round(r.left + r.width / 2) + 'px');
      t.style.setProperty('--tip-y', Math.round(below ? r.bottom : r.top) + 'px');
      t.setAttribute('data-show', '');
    };

    const hide = () => {
      current = null;
      if (tip) tip.removeAttribute('data-show');
    };

    // 事件委托：后续新增的 data-tip 元素自动生效，不需要重新绑定
    document.addEventListener('pointerover', (e) => {
      const el = e.target.closest('[data-tip]');
      if (el && el !== current) show(el);
      else if (!el && current) hide();
    });
    document.addEventListener('pointerdown', hide);   // 点下去就该收起来
    document.addEventListener('focusin', (e) => {
      const el = e.target.closest('[data-tip]');
      el ? show(el) : hide();
    });
    document.addEventListener('focusout', hide);
    document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hide(); });
    // fixed 定位不会跟着页面滚，滚动时直接收起比让它飘在错位置上好
    addEventListener('scroll', hide, { passive: true, capture: true });
    addEventListener('resize', hide, { passive: true });
  })();


  // 4. 语言菜单：把并排的链接收成一个下拉
  //
  // 服务端渲染的是几个并排链接——没有 JS 时那本身就是完全可用的形态，
  // 不需要任何降级分支。这里只是把它折起来，省掉导航栏上的横向空间。
  for (const menu of document.querySelectorAll('[data-lang-menu]')) {
    enhanceLangMenu(menu);
  }

  function enhanceLangMenu(menu) {
    const links = [...menu.querySelectorAll('a')];
    const current = menu.querySelector('[data-lang-current]');
    if (links.length < 2 || !current) return;

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'menu-btn';
    btn.setAttribute('aria-haspopup', 'listbox');
    btn.setAttribute('aria-expanded', 'false');
    btn.setAttribute('aria-label', menu.dataset.label || 'Language');
    // 直接搬运服务端渲染好的内容（地球图标 + 当前语言名），
    // 不在 JS 里重新拼一遍——拼的那份迟早和模板走偏。
    while (current.firstChild) btn.appendChild(current.firstChild);

    const pop = document.createElement('div');
    pop.className = 'menu-pop';
    pop.hidden = true;

    // 超过这个数量就给个过滤框。十来种语言靠滚动还能找，
    // 五十种不给搜索就等于让人一行行扫——而且他要找的那种语言，
    // 他多半只认得出它的母语名，拼不出英文名。
    const FILTER_AT = 8;
    let filter = null;
    if (links.length > FILTER_AT) {
      filter = document.createElement('input');
      filter.type = 'search';
      filter.className = 'lang-filter';
      filter.placeholder = menu.dataset.filter || '';
      filter.setAttribute('aria-label', menu.dataset.filter || 'Filter');
      pop.appendChild(filter);
    }

    const list = document.createElement('ul');
    list.className = 'lang-list';
    list.setAttribute('role', 'listbox');
    const items = links.map((a) => {
      const li = document.createElement('li');
      a.classList.add('menu-option');
      a.setAttribute('role', 'option');
      if (a.hasAttribute('data-current-lang')) {
        a.setAttribute('aria-selected', 'true');
      } else {
        a.setAttribute('aria-selected', 'false');
      }
      li.appendChild(a);
      list.appendChild(li);
      return { li, a, text: (a.textContent + ' ' + (a.getAttribute('hreflang') || '')).toLowerCase() };
    });
    pop.appendChild(list);
    menu.append(btn, pop);
    menu.setAttribute('data-enhanced', '');

    let active = Math.max(0, items.findIndex((i) => i.a.hasAttribute('data-current-lang')));

    const visible = () => items.filter((i) => !i.li.hidden);
    const mark = () => {
      for (const i of items) i.a.toggleAttribute('data-active', items.indexOf(i) === active);
    };
    const applyFilter = () => {
      const q = filter ? filter.value.trim().toLowerCase() : '';
      for (const i of items) i.li.hidden = q !== '' && !i.text.includes(q);
      const vis = visible();
      if (vis.length && !vis.includes(items[active])) active = items.indexOf(vis[0]);
      mark();
    };

    const open = () => {
      pop.hidden = false;
      btn.setAttribute('aria-expanded', 'true');
      applyFilter();
      if (filter) filter.focus();
      const cur = items[active];
      if (cur) cur.a.scrollIntoView({ block: 'nearest' });
    };
    const close = () => {
      pop.hidden = true;
      btn.setAttribute('aria-expanded', 'false');
      if (filter) filter.value = '';
    };

    const move = (d) => {
      const vis = visible();
      if (!vis.length) return;
      let idx = vis.indexOf(items[active]);
      idx = Math.max(0, Math.min(vis.length - 1, (idx < 0 ? 0 : idx) + d));
      active = items.indexOf(vis[idx]);
      mark();
      vis[idx].a.scrollIntoView({ block: 'nearest' });
    };

    btn.addEventListener('click', () => (pop.hidden ? open() : close()));
    btn.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
      else if (e.key === 'Escape') close();
    });

    pop.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { close(); btn.focus(); }
      else if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
      else if (e.key === 'Enter' && filter && e.target === filter) {
        // 在过滤框里回车 = 选中当前高亮项，不用再按方向键挪过去
        e.preventDefault();
        const cur = items[active];
        if (cur && !cur.li.hidden) cur.a.click();
      }
    });
    if (filter) filter.addEventListener('input', applyFilter);
    list.addEventListener('mousemove', (e) => {
      const a = e.target.closest('.menu-option');
      if (!a) return;
      const i = items.findIndex((x) => x.a === a);
      if (i >= 0 && i !== active) { active = i; mark(); }
    });

    document.addEventListener('pointerdown', (e) => {
      if (!pop.hidden && !menu.contains(e.target)) close();
    });
    document.addEventListener('focusin', (e) => {
      if (!pop.hidden && !menu.contains(e.target)) close();
    });
    mark();
  }


  // 4b. 账号菜单：把管理/退出收成一个下拉
  //
  // 和语言菜单同构，但简单得多：没有过滤框、没有滚动、项数固定。
  // 关键差别是"退出"是个 POST 表单而不是链接——搬运的是服务端渲染好的
  // 元素本身，不在 JS 里重建成 <a>。GET 的登出会被浏览器预取器直接触发。
  for (const menu of document.querySelectorAll('[data-menu]')) {
    enhanceMenu(menu);
  }

  function enhanceMenu(menu) {
    const current = menu.querySelector('[data-menu-current]');
    if (!current) return;
    const rest = [...menu.children].filter((el) => el !== current);
    if (!rest.length) return;

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'menu-btn';
    btn.setAttribute('aria-haspopup', 'menu');
    btn.setAttribute('aria-expanded', 'false');
    if (menu.dataset.label) btn.setAttribute('aria-label', menu.dataset.label);
    while (current.firstChild) btn.appendChild(current.firstChild);

    const pop = document.createElement('div');
    pop.className = 'menu-pop';
    pop.setAttribute('role', 'menu');
    pop.hidden = true;
    for (const el of rest) pop.appendChild(el);

    const items = [...pop.querySelectorAll('a, button')];
    for (const it of items) it.setAttribute('role', 'menuitem');

    menu.append(btn, pop);
    menu.setAttribute('data-enhanced', '');

    let active = 0;
    const mark = () => {
      items.forEach((it, i) => it.toggleAttribute('data-active', i === active));
    };
    const open = () => {
      pop.hidden = false;
      btn.setAttribute('aria-expanded', 'true');
      active = 0;
      mark();
      items[0].focus();
    };
    const close = (back) => {
      pop.hidden = true;
      btn.setAttribute('aria-expanded', 'false');
      for (const it of items) it.removeAttribute('data-active');
      if (back) btn.focus();
    };
    const move = (d) => {
      active = (active + d + items.length) % items.length;
      mark();
      items[active].focus();
    };

    btn.addEventListener('click', () => (pop.hidden ? open() : close()));
    btn.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
    });
    pop.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { close(true); }
      else if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
    });
    document.addEventListener('pointerdown', (e) => {
      if (!pop.hidden && !menu.contains(e.target)) close();
    });
    document.addEventListener('focusin', (e) => {
      if (!pop.hidden && !menu.contains(e.target)) close();
    });
  }


  // 4c. 把 <details> 升级成弹窗
  //
  // 服务端渲染的是 <details><summary>：没有 JS 时点一下就地展开，表单
  // 完全可用。这里把它搬进原生 <dialog>——不引任何库，Esc 关闭、焦点
  // 陷阱、遮罩都是浏览器自带的。
  for (const d of document.querySelectorAll('details[data-dialog]')) {
    enhanceDialog(d);
  }

  function enhanceDialog(det) {
    const summary = det.querySelector('summary');
    const body = [...det.children].filter((el) => el !== summary);
    if (!summary || !body.length) return;

    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = summary.className;
    btn.textContent = summary.textContent;

    const dlg = document.createElement('dialog');
    dlg.className = 'dialog';
    const head = document.createElement('div');
    head.className = 'dialog-head';
    const h = document.createElement('p');
    h.className = 'dialog-title';
    h.textContent = det.dataset.title || summary.textContent;
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'dialog-close';
    x.setAttribute('aria-label', det.dataset.close || 'Close');
    x.textContent = '\u00d7';
    head.append(h, x);
    dlg.appendChild(head);
    for (const el of body) dlg.appendChild(el);

    det.replaceWith(btn);
    document.body.appendChild(dlg);

    btn.addEventListener('click', () => dlg.showModal());
    x.addEventListener('click', () => dlg.close());
    // 点遮罩关闭：<dialog> 自己就是那块遮罩，点在它自身而不是内容上时才算
    dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
  }


  // 5. 自定义下拉
  //
  // 原生 <select> 展开的列表由操作系统绘制，CSS 完全管不到，跟站点其余
  // 部分对不上。这里在它外面套一层可见的按钮 + 列表，原生控件本身留在
  // 表单里（只是移出视觉流）：关掉 JS 就是一个完整可用的原生下拉，
  // 不需要任何降级分支；提交、重置、自动填充也都还走它。
  for (const sel of document.querySelectorAll('select:not([multiple]):not([data-native])')) {
    enhanceSelect(sel);
  }

  function enhanceSelect(sel) {
    const opts = [...sel.options];
    if (!opts.length) return;

    const wrap = document.createElement('div');
    wrap.className = 'select';
    sel.parentNode.insertBefore(wrap, sel);
    wrap.appendChild(sel);
    sel.classList.add('select-native');
    sel.setAttribute('tabindex', '-1');
    sel.setAttribute('aria-hidden', 'true');

    const btn = document.createElement('button');
    btn.type = 'button';                 // 默认是 submit，不写会把表单提交掉
    btn.className = 'select-btn';
    btn.setAttribute('aria-haspopup', 'listbox');
    btn.setAttribute('aria-expanded', 'false');
    // 按钮的文字是"当前值"，而读屏用户需要知道的是"这个控件管什么"。
    // 把原生 select 的 aria-label，或者包着它的 <label> 的文字，挪过来。
    const purpose = sel.getAttribute('aria-label') ||
      (sel.closest('label') && sel.closest('label').querySelector('.label')
        ? sel.closest('label').querySelector('.label').textContent.trim() : '');
    if (purpose) btn.setAttribute('aria-label', purpose);

    const label = document.createElement('span');
    btn.append(label, svg('caret'));

    const list = document.createElement('ul');
    list.className = 'select-list';
    list.setAttribute('role', 'listbox');
    list.id = 'sel-' + Math.random().toString(36).slice(2, 9);
    if (purpose) list.setAttribute('aria-label', purpose);
    btn.setAttribute('aria-controls', list.id);
    list.hidden = true;

    const items = opts.map((o, idx) => {
      const li = document.createElement('li');
      li.className = 'select-option';
      li.setAttribute('role', 'option');
      li.dataset.index = idx;
      const text = document.createElement('span');
      text.textContent = o.textContent;
      li.append(svg('check'), text);
      list.appendChild(li);
      return li;
    });

    wrap.append(btn, list);

    let active = sel.selectedIndex < 0 ? 0 : sel.selectedIndex;

    const sync = () => {
      const cur = opts[sel.selectedIndex];
      label.textContent = cur ? cur.textContent : '';
      // aria-label 是用途，值靠按钮文字播报；两者合起来就是
      // "按状态筛选，全部状态"，这正是原生 select 的播报方式。
      items.forEach((li, i) => {
        li.setAttribute('aria-selected', String(i === sel.selectedIndex));
        li.toggleAttribute('data-active', i === active);
      });
    };

    const open = () => {
      active = sel.selectedIndex < 0 ? 0 : sel.selectedIndex;
      list.hidden = false;
      wrap.setAttribute('data-open', '');
      btn.setAttribute('aria-expanded', 'true');
      sync();
      items[active] && items[active].scrollIntoView({ block: 'nearest' });
    };
    const close = () => {
      list.hidden = true;
      wrap.removeAttribute('data-open');
      btn.setAttribute('aria-expanded', 'false');
    };
    const isOpen = () => !list.hidden;

    const pick = (i) => {
      sel.selectedIndex = i;
      // 派发 change，让监听原生 select 的代码照常工作
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      sync();
      close();
      btn.focus();
    };

    const move = (delta) => {
      active = Math.max(0, Math.min(items.length - 1, active + delta));
      sync();
      items[active].scrollIntoView({ block: 'nearest' });
    };

    btn.addEventListener('click', () => (isOpen() ? close() : open()));

    btn.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp' || e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        isOpen() ? move(e.key === 'ArrowUp' ? -1 : 1) : open();
      } else if (e.key === 'Escape' && isOpen()) {
        close();
      } else if (isOpen() && e.key === 'Tab') {
        close();
      }
    });

    // Enter 在展开状态下要落在"当前高亮项"上，所以单独处理一次 keyup
    btn.addEventListener('keyup', (e) => {
      if (isOpen() && (e.key === 'Enter' || e.key === ' ')) {
        e.preventDefault();
        pick(active);
      }
    });

    list.addEventListener('mousedown', (e) => {
      const li = e.target.closest('.select-option');
      if (li) { e.preventDefault(); pick(+li.dataset.index); }
    });
    list.addEventListener('mousemove', (e) => {
      const li = e.target.closest('.select-option');
      if (li && +li.dataset.index !== active) { active = +li.dataset.index; sync(); }
    });

    document.addEventListener('pointerdown', (e) => {
      if (isOpen() && !wrap.contains(e.target)) close();
    });
    // 表单重置后原生 select 会回到默认值，可见层要跟上
    if (sel.form) sel.form.addEventListener('reset', () => setTimeout(sync));

    sync();
  }

  // svg 生成下拉用的两个小图标。用 createElementNS 而不是 innerHTML，
  // 省掉一次 HTML 解析，也避免把字符串拼接引进渲染路径。
  function svg(kind) {
    const NS = 'http://www.w3.org/2000/svg';
    const el = document.createElementNS(NS, 'svg');
    el.setAttribute('viewBox', '0 0 16 16');
    el.setAttribute('fill', 'none');
    el.setAttribute('stroke', 'currentColor');
    el.setAttribute('stroke-width', '1.8');
    el.setAttribute('stroke-linecap', 'round');
    el.setAttribute('stroke-linejoin', 'round');
    el.setAttribute('aria-hidden', 'true');
    el.setAttribute('class', kind === 'caret' ? 'select-caret' : 'select-check');
    const path = document.createElementNS(NS, 'path');
    path.setAttribute('d', kind === 'caret' ? 'M4 6.5 8 10.5 12 6.5' : 'M3.5 8.5 6.5 11.5 12.5 4.5');
    el.appendChild(path);
    return el;
  }


  // 7. 站标：小圆朝着光标的方向绕环公转
  //
  // 几个刻意的选择：
  //  - 改的是 SVG 的 cx/cy 几何属性，不是 CSS transform。站点 CSP 是
  //    style-src 'self'，动内联样式是给自己找麻烦；几何属性不受它约束。
  //  - 每帧只写两个属性，用 requestAnimationFrame 合并，不在 pointermove
  //    里直接改 DOM——鼠标事件的频率可以远高于刷新率。
  //  - 角度差取最短弧。不取的话，从 179° 挪到 -179° 会绕整整一圈回来。
  (() => {
    // 只取顶部那一枚：页脚也有一个同样的标记，但两个同时跟着鼠标转
    // 会互相抢注意力，而页脚本来就是让视线落下来的地方。
    const svg = document.querySelector('.site-header .brand-mark');
    const disc = svg && svg.querySelector('.mark-disc');
    if (!disc) return;

    // 触屏没有"光标方向"可言，动了也没人看见；
    // 用户要求减少动效时同样不跑。两种情况下小圆停在设计稿的静止位。
    if (!matchMedia('(hover: hover) and (pointer: fine)').matches) return;
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const CX = 17, CY = 17, R = 10.61;   // 环心与公转半径，与模板里的数值对应
    const REST = Math.PI / 4;            // 静止时指向右下，即固定版的位置
    let target = REST, current = REST, raf = 0;

    const place = (a) => {
      disc.setAttribute('cx', (CX + R * Math.cos(a)).toFixed(2));
      disc.setAttribute('cy', (CY + R * Math.sin(a)).toFixed(2));
    };

    // 排帧前先取消上一帧。
    //
    // 不能拿 raf 这个 id 当"循环在跑"的标志去判断要不要排队：一旦某次
    // requestAnimationFrame 排了队却永远不执行（页面切到后台、窗口停止
    // 合成），标志就永久卡在真值，之后所有 aim() 都会以为循环还活着而
    // 不再排队，动画彻底死掉且不报任何错。先 cancel 再 schedule 就没有
    // 这种状态——一帧之内来多少次 pointermove，最终也只会执行最后排的
    // 那一个回调，合并效果和用标志判断是一样的。
    const schedule = () => {
      if (raf) cancelAnimationFrame(raf);
      raf = requestAnimationFrame(tick);
    };

    const tick = () => {
      raf = 0;                           // 回调一进来就清掉，标志永远不会陈旧
      let d = target - current;
      while (d > Math.PI) d -= 2 * Math.PI;
      while (d < -Math.PI) d += 2 * Math.PI;
      if (Math.abs(d) < 0.003) return;   // 追上了就停，别让页面无事发生也每帧跑
      current += d * 0.16;               // 缓动系数：越小越黏，越大越跳
      place(current);
      schedule();
    };

    const aim = (a) => {
      target = a;
      schedule();
    };

    addEventListener('pointermove', (e) => {
      const b = svg.getBoundingClientRect();
      if (!b.width) return;              // 标记被隐藏时不算
      aim(Math.atan2(e.clientY - (b.top + b.height / 2),
                     e.clientX - (b.left + b.width / 2)));
    }, { passive: true });

    // 鼠标离开窗口、或页面切到后台时回到静止位，
    // 否则再回来时小圆会僵在一个莫名其妙的角度上
    document.addEventListener('pointerleave', () => aim(REST));
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) return;
      if (raf) { cancelAnimationFrame(raf); raf = 0; }
      current = target = REST;
      place(REST);
    });
  })();


  // 8. token 明文点击复制
  // 文案从 data- 属性来，不在 JS 里写死：这里写死的话十种界面语言下
  // 它永远是中文，而且 lang check 查不出来——它不在模板里。
  for (const el of document.querySelectorAll('.copyable')) {
    el.title = el.dataset.copy || '';
    el.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(el.textContent.trim());
        const old = el.textContent;
        el.textContent = el.dataset.copied || old;
        setTimeout(() => { el.textContent = old; }, 1200);
      } catch (_) { /* 非安全上下文下 clipboard 不可用，用户可手动选中 */ }
    });
  }

  // 按钮式复制：复制 data-copy-text，把 tips 临时换成"已复制"
  for (const btn of document.querySelectorAll('[data-copy-text]')) {
    btn.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(btn.dataset.copyText);
        const old = btn.dataset.tip;
        btn.dataset.tip = btn.dataset.copied || old;
        setTimeout(() => { btn.dataset.tip = old; }, 1200);
      } catch (_) { /* 同上 */ }
    });
  }
})();
