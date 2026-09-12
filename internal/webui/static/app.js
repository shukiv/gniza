// Progressive enhancement only: every page works with JavaScript disabled.
(function () {
  "use strict";

  // Account filtering happens here rather than on the server: the list is
  // already rendered, and a round trip per keystroke would be slower and
  // would cost a page load for something the browser can do instantly.
  function rowsOf(selector) {
    var table = document.querySelector(selector);
    return table ? Array.prototype.slice.call(table.querySelectorAll("tbody tr")) : [];
  }

  function applyFilters(selector) {
    var search = document.querySelector('[data-filter="' + selector + '"]');
    var chips = document.querySelector('[data-filter-state="' + selector + '"]');
    var term = search ? search.value.trim().toLowerCase() : "";
    var state = "";
    if (chips) {
      var pressed = chips.querySelector('button[aria-pressed="true"]');
      state = pressed ? pressed.dataset.state : "";
    }

    var shown = 0;
    var table = document.querySelector(selector);
    rowsOf(selector).forEach(function (row) {
      var name = (row.dataset.name || "").toLowerCase();
      var matchesTerm = !term || name.indexOf(term) !== -1;
      var matchesState = !state || row.dataset.state === state;
      var visible = matchesTerm && matchesState;
      // Two things hide a row — the filter and the page it is not on —
      // and each has to know what the other decided. The filter records
      // its verdict here; the paginator has the last word on .hidden.
      row.dataset.filtered = visible ? "" : "1";
      row.hidden = !visible;
      if (visible) { shown++; }
    });

    var counter = document.querySelector('[data-count-for="' + selector + '"]');
    if (counter) { counter.textContent = String(shown); }

    if (table && typeof window.gnizaRepaginate === "function") {
      // A filtered list is a shorter list: back to the first page, or the
      // operator is looking at page four of a single match.
      window.gnizaRepaginate(table, true);
    }
  }

  Array.prototype.forEach.call(document.querySelectorAll("[data-filter]"), function (input) {
    var target = input.dataset.filter;
    input.addEventListener("input", function () { applyFilters(target); });
  });

  // A live refresh replaces rows, which loses the filter the operator set.
  // Re-applying it is the one thing outside this closure needs from here.
  window.gnizaReapplyFilters = function () {
    Array.prototype.forEach.call(document.querySelectorAll("[data-filter]"), function (input) {
      applyFilters(input.dataset.filter);
    });
  };

  Array.prototype.forEach.call(document.querySelectorAll("[data-filter-state]"), function (group) {
    var target = group.dataset.filterState;
    group.addEventListener("click", function (event) {
      var chip = event.target.closest("button[aria-pressed]");
      if (!chip) { return; }
      Array.prototype.forEach.call(group.querySelectorAll("button[aria-pressed]"), function (other) {
        other.setAttribute("aria-pressed", String(other === chip));
      });
      applyFilters(target);
    });
  });

  // The administrator to log in as only matters when Gniza is being
  // asked to create the account.
  var createAccount = document.querySelector("[data-quick-create]");
  var adminFields = document.querySelector("[data-quick-admin]");
  var passwordLabel = document.querySelector("[data-quick-password-label]");
  if (createAccount) {
    createAccount.addEventListener("change", function () {
      if (adminFields) { adminFields.hidden = !createAccount.checked; }
      // The same field means two different passwords, so it says which.
      if (passwordLabel) {
        passwordLabel.textContent = createAccount.checked
          ? "Administrator's password on that server"
          : "Password for that account";
      }
    });
  }

  // The recommended excludes are added to whatever is already there, and
  // only the lines that are not there yet — pressing the same one twice
  // should not fill the box with duplicates.
  var excludes = document.querySelector("[data-excludes]");
  Array.prototype.forEach.call(document.querySelectorAll("[data-exclude-preset]"), function (button) {
    button.addEventListener("click", function () {
      if (!excludes) { return; }
      var have = excludes.value.split("\n").map(function (line) { return line.trim(); });
      button.dataset.excludePreset.split("\n").forEach(function (line) {
        if (line && have.indexOf(line) === -1) {
          have.push(line);
        }
      });
      excludes.value = have.filter(function (line) { return line !== ""; }).join("\n");
      excludes.focus();
    });
  });

  // Picking files to restore. The list of what is chosen has to survive
  // walking into another folder, which is a page load, so it lives in the
  // textarea — the same field someone can still type into — and is kept
  // across those loads in this tab's own storage.
  var chosen = document.querySelector("[data-chosen-paths]");
  if (chosen) {
    var key = "gniza.chosen." + (chosen.dataset.chosenKey || "");

    var remember = function () {
      try { window.sessionStorage.setItem(key, chosen.value); } catch (e) {}
      var count = chosen.value.split("\n").filter(function (line) {
        return line.trim() !== "";
      }).length;
      Array.prototype.forEach.call(document.querySelectorAll("[data-chosen-count]"), function (node) {
        node.textContent = String(count);
      });
    };

    try {
      var kept = window.sessionStorage.getItem(key);
      if (kept && chosen.value === "") { chosen.value = kept; }
    } catch (e) {}
    remember();
    chosen.addEventListener("input", remember);

    var boxes = function () {
      return Array.prototype.slice.call(document.querySelectorAll("[data-choose]"));
    };

    var all = document.querySelector("[data-choose-all]");
    if (all) {
      all.addEventListener("change", function () {
        boxes().forEach(function (box) { box.checked = all.checked; });
      });
    }

    var add = document.querySelector("[data-choose-add]");
    if (add) {
      add.addEventListener("click", function () {
        var lines = chosen.value.split("\n").map(function (line) { return line.trim(); });
        boxes().forEach(function (box) {
          if (box.checked && lines.indexOf(box.dataset.choose) === -1) {
            lines.push(box.dataset.choose);
            box.checked = false;
          }
        });
        if (all) { all.checked = false; }
        chosen.value = lines.filter(function (line) { return line !== ""; }).join("\n");
        remember();
      });
    }

    // Once the restore is queued the list has served its purpose; leaving
    // it would repopulate the next one with the last one's paths.
    var form = chosen.form;
    if (form) {
      form.addEventListener("submit", function () {
        try { window.sessionStorage.removeItem(key); } catch (e) {}
      });
    }
  }

  // Show the fields that belong to the chosen destination type.
  var typeSelect = document.querySelector("[data-type-toggle]");
  if (typeSelect) {
    var syncType = function () {
      Array.prototype.forEach.call(document.querySelectorAll("[data-for-type]"), function (block) {
        var applies = block.getAttribute("data-for-type") === typeSelect.value;
        block.hidden = !applies;
        Array.prototype.forEach.call(block.querySelectorAll("input, select"), function (field) {
          // A hidden required field would block submission invisibly.
          field.disabled = !applies;
        });
      });
    };
    typeSelect.addEventListener("change", syncType);
    syncType();
  }

  // Reveal the account picker only when a schedule is not "all accounts".
  // On a server without a panel there is no picker and no choice of
  // scope: the choosing tables are the form, and what is ticked is
  // what the schedule covers.
  Array.prototype.forEach.call(document.querySelectorAll("[data-scope-toggle]"), function (form) {
    var picker = form.querySelector("[data-account-picker]");
    if (!picker) { return; }
    var syncScope = function () {
      var selected = form.querySelector("input[name=scope]:checked");
      picker.hidden = !selected || selected.value !== "selected";
    };
    Array.prototype.forEach.call(form.querySelectorAll("input[name=scope]"), function (radio) {
      radio.addEventListener("change", syncScope);
    });
    syncScope();
  });

  // Ask before anything that overwrites a live account.
  // A form asks before it submits; a button asks before it submits the
  // form it is in. The difference matters where one form has two buttons
  // and only one of them is destructive — rebuilding an archive needs no
  // confirmation, handing it to cPanel's restore does.
  Array.prototype.forEach.call(document.querySelectorAll("[data-confirm]"), function (element) {
    var when = element.tagName === "FORM" ? "submit" : "click";
    element.addEventListener(when, function (event) {
      if (!window.confirm(element.getAttribute("data-confirm"))) {
        event.preventDefault();
      }
    });
  });

  // Tabs, and tables with a box per row. Both live in forms the drawer
  // may fetch after this has run, so the page listens for them rather
  // than each control, and looks again when the drawer says it loaded.
  //
  // Tabs: the sections stack one under the other without script; with
  // it, one shows at a time and the tab strip says which. Boxes: the
  // header's box ticks the column, a stack's box ticks its rows, and
  // the button counts what is ticked so "Back these up" says how many.
  function showTab(root, name) {
    root.dataset.tabs = name;
    var remember = root.closest("form") && root.closest("form").querySelector("input[name=tab]");
    if (remember) { remember.value = name; }
    root.querySelectorAll("[data-tab]").forEach(function (tab) {
      var active = tab.dataset.tab === name;
      tab.classList.toggle("cpr:tab-active", active);
      tab.setAttribute("aria-selected", active ? "true" : "false");
      tab.setAttribute("tabindex", active ? "0" : "-1");
    });
    root.querySelectorAll("[data-tab-panel]").forEach(function (panel) {
      panel.hidden = panel.dataset.tabPanel !== name;
    });
  }
  function rowBoxes(table) {
    return Array.prototype.filter.call(table.querySelectorAll("input[type=checkbox]"), function (box) {
      return !box.hasAttribute("data-tick-all") && !box.hasAttribute("data-tick-group") && !box.disabled;
    });
  }
  function settle(table) {
    var boxes = rowBoxes(table);
    var ticked = boxes.filter(function (box) { return box.checked; }).length;
    var all = table.querySelector("[data-tick-all]");
    if (all) {
      all.checked = boxes.length > 0 && ticked === boxes.length;
      all.indeterminate = ticked > 0 && ticked < boxes.length;
    }
    table.querySelectorAll("[data-tick-group]").forEach(function (group) {
      var own = boxes.filter(function (box) {
        var row = box.closest("tr");
        return row && row.dataset.group === group.dataset.tickGroup;
      });
      var on = own.filter(function (box) { return box.checked; }).length;
      group.checked = own.length > 0 && on === own.length;
      group.indeterminate = on > 0 && on < own.length;
    });
    var form = table.closest("form");
    var button = form && form.querySelector("[data-tick-count]");
    if (button) {
      var across = 0;
      form.querySelectorAll("[data-tickable]").forEach(function (other) {
        across += rowBoxes(other).filter(function (box) { return box.checked; }).length;
      });
      if (!button.dataset.label) { button.dataset.label = button.textContent; }
      button.textContent = across > 0 ? button.dataset.label + " (" + across + ")" : button.dataset.label;
    }
  }
  function prepare(scope) {
    scope.querySelectorAll("[data-tabs]").forEach(function (root) { showTab(root, root.dataset.tabs); });
    scope.querySelectorAll("[data-tickable]").forEach(settle);
  }
  document.addEventListener("click", function (event) {
    var tab = event.target.closest("[data-tab]");
    if (!tab) { return; }
    var root = tab.closest("[data-tabs]");
    if (root) { showTab(root, tab.dataset.tab); }
  });
  document.addEventListener("keydown", function (event) {
    var tab = event.target.closest && event.target.closest("[data-tab]");
    if (!tab || (event.key !== "ArrowLeft" && event.key !== "ArrowRight")) { return; }
    var tabs = Array.prototype.slice.call(tab.parentNode.querySelectorAll("[data-tab]"));
    var next = tabs[(tabs.indexOf(tab) + (event.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
    event.preventDefault();
    next.focus();
    next.click();
  });
  document.addEventListener("change", function (event) {
    var box = event.target;
    if (!box.matches || !box.matches("input[type=checkbox]")) { return; }
    var table = box.closest("[data-tickable]");
    if (!table) { return; }
    if (box.hasAttribute("data-tick-all")) {
      rowBoxes(table).forEach(function (row) { row.checked = box.checked; });
    } else if (box.hasAttribute("data-tick-group")) {
      rowBoxes(table).forEach(function (row) {
        var tr = row.closest("tr");
        if (tr && tr.dataset.group === box.dataset.tickGroup) { row.checked = box.checked; }
      });
    }
    // One source can stand behind several rows, on several tabs: a
    // folder chosen with its databases is one name. They tick together.
    if (box.dataset.same) {
      var root = box.closest("[data-tabs]") || table;
      root.querySelectorAll("input[data-same]").forEach(function (other) {
        if (other.dataset.same === box.dataset.same) { other.checked = box.checked; }
      });
      root.querySelectorAll("[data-tickable]").forEach(settle);
      return;
    }
    settle(table);
  });
  // The folder browser on the folders tab: one directory at a time,
  // read from the service as data, with a box on each folder. A box
  // ticked puts the folder in the table above it as a row that posts
  // "folder", so what was ticked stays while the browser moves on.
  function pickedRow(table, path) {
    return Array.prototype.find.call(table.querySelectorAll("input[name=folder]"), function (box) { return box.value === path; });
  }
  function pick(browser, path, on) {
    var form = browser.closest("form");
    var table = form && form.querySelector("[data-picked]");
    if (!table) { return; }
    var box = pickedRow(table, path);
    if (!box) {
      var row = document.createElement("tr");
      var cell = document.createElement("td");
      box = document.createElement("input");
      box.type = "checkbox"; box.className = "cpr:checkbox cpr:checkbox-sm"; box.name = "folder"; box.value = path;
      box.setAttribute("aria-label", path);
      cell.appendChild(box); row.appendChild(cell);
      var name = document.createElement("td"); name.className = "cpr:mono"; name.textContent = path; row.appendChild(name);
      var as = document.createElement("td"); as.innerHTML = '<span class="cpr:text-base-content/50">when saved</span>'; row.appendChild(as);
      var empty = table.querySelector("[data-picked-empty]");
      table.querySelector("tbody").insertBefore(row, empty);
      if (empty) { empty.hidden = true; }
    }
    box.checked = on;
    settle(table);
  }
  function drawListing(browser, listing) {
    browser.dataset.dir = listing.Dir;
    var form = browser.closest("form");
    var picked = form && form.querySelector("[data-picked]");
    var crumbs = browser.querySelector("[data-crumbs]");
    crumbs.textContent = "";
    var parts = listing.Dir === "/" ? [] : listing.Dir.replace(/^\//, "").split("/");
    var path = "";
    [{ name: "/", path: "/" }].concat(parts.map(function (part) { path += "/" + part; return { name: part, path: path }; })).forEach(function (step, i) {
      if (i) { var slash = document.createElement("span"); slash.className = "cpr:text-base-content/40"; slash.textContent = "/"; crumbs.appendChild(slash); }
      var button = document.createElement("button");
      button.type = "button"; button.className = "cpr:btn cpr:btn-xs cpr:btn-ghost cpr:mono"; button.dataset.go = step.path; button.textContent = step.name;
      crumbs.appendChild(button);
    });
    var label = browser.querySelector("[data-crumb-dir]");
    if (label) { label.textContent = listing.Dir; }
    var body = browser.querySelector("[data-entries]");
    body.textContent = "";
    (listing.Entries || []).forEach(function (entry) {
      var row = document.createElement("tr");
      var cell = document.createElement("td");
      var box = document.createElement("input");
      box.type = "checkbox"; box.className = "cpr:checkbox cpr:checkbox-sm"; box.setAttribute("aria-label", entry.Path);
      if (entry.ChosenAs) { box.checked = true; box.disabled = true; }
      else {
        box.dataset.pick = entry.Path;
        var already = picked && pickedRow(picked, entry.Path);
        box.checked = !!(already && already.checked);
      }
      cell.appendChild(box); row.appendChild(cell);
      var name = document.createElement("td");
      var open = document.createElement("button");
      open.type = "button"; open.className = "cpr:link cpr:link-hover cpr:mono"; open.dataset.go = entry.Path; open.textContent = entry.Name + "/";
      name.appendChild(open); row.appendChild(name);
      var as = document.createElement("td");
      if (entry.ChosenAs) { as.textContent = entry.ChosenAs; } else { as.innerHTML = '<span class="cpr:text-base-content/50">not yet</span>'; }
      row.appendChild(as);
      body.appendChild(row);
    });
    if (!(listing.Entries || []).length) {
      var none = document.createElement("tr");
      none.innerHTML = '<td colspan="3" class="cpr:text-base-content/50">No folder under it.</td>';
      body.appendChild(none);
    }
  }
  function browse(browser, dir) {
    var url = browser.dataset.browse + (browser.dataset.browse.indexOf("?") >= 0 ? "&" : "?") + "dir=" + encodeURIComponent(dir);
    fetch(url, { headers: { "Accept": "application/json" }, credentials: "same-origin" })
      .then(function (response) { return response.json().then(function (data) { return { ok: response.ok, data: data }; }); })
      .then(function (result) {
        if (!result.ok) { throw new Error(result.data && result.data.error ? result.data.error : "could not read the folder"); }
        drawListing(browser, result.data);
      })
      .catch(function (err) {
        var body = browser.querySelector("[data-entries]");
        body.textContent = "";
        var row = document.createElement("tr");
        var cell = document.createElement("td"); cell.colSpan = 3; cell.className = "cpr:text-error"; cell.textContent = String(err.message || err);
        row.appendChild(cell); body.appendChild(row);
      });
  }
  document.addEventListener("click", function (event) {
    var go = event.target.closest("[data-go]");
    if (!go) { return; }
    var browser = go.closest("[data-folder-browser]");
    if (browser) { browse(browser, go.dataset.go); }
  });
  document.addEventListener("change", function (event) {
    var box = event.target;
    if (!box.matches || !box.matches("input[type=checkbox][data-pick]")) { return; }
    var browser = box.closest("[data-folder-browser]");
    if (browser) { pick(browser, box.dataset.pick, box.checked); }
  });
  // A browser drawn without a listing, on a page the drawer fetched
  // later for instance, reads its directory now.
  function wake(scope) {
    scope.querySelectorAll("[data-folder-browser]").forEach(function (browser) {
      if (browser.querySelector("[data-entries-empty]")) { browse(browser, browser.dataset.dir || "/"); }
    });
  }
  document.addEventListener("gniza:loaded", function (event) { prepare(event.detail || document); wake(event.detail || document); });
  prepare(document);
  wake(document);

  // Row menus.
  //
  // The panel is in normal flow to begin with, so it works with no
  // JavaScript. That costs something when the script is there: an open
  // panel widened the table, which pushed a horizontal scrollbar under
  // the page and shoved the row out from under the cursor. Script lifts
  // it out of the flow instead — fixed, positioned against the button —
  // which no scroll container can clip and no layout has to make room
  // for. It goes back into the flow when the menu closes.
  function placeMenu(menu) {
    var panel = menu.querySelector("[data-menu-body]");
    var button = menu.querySelector("summary");
    if (!panel || !button) { return; }
    var rect = button.getBoundingClientRect();
    panel.style.position = "fixed";
    panel.style.top = Math.round(rect.bottom + 4) + "px";
    panel.style.left = "auto";
    panel.style.right = Math.round(window.innerWidth - rect.right) + "px";
    panel.style.margin = "0";
    panel.style.zIndex = "60";
  }

  function unplaceMenu(menu) {
    var panel = menu.querySelector("[data-menu-body]");
    if (panel) { panel.removeAttribute("style"); }
  }

  function closeMenus(except) {
    Array.prototype.forEach.call(document.querySelectorAll("details[data-menu][open]"), function (menu) {
      if (menu === except) { return; }
      menu.open = false;
      unplaceMenu(menu);
    });
  }

  Array.prototype.forEach.call(document.querySelectorAll("details[data-menu]"), function (menu) {
    menu.addEventListener("toggle", function () {
      if (menu.open) {
        closeMenus(menu);
        placeMenu(menu);
      } else {
        unplaceMenu(menu);
      }
    });
  });

  // A panel pinned to the viewport does not follow the button it belongs
  // to. Rather than track it, close it: the button is still there.
  window.addEventListener("scroll", function () { closeMenus(null); }, true);
  window.addEventListener("resize", function () { closeMenus(null); });

  document.addEventListener("click", function (event) {
    closeMenus(event.target.closest("details[data-menu]"));
  });
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") { return; }
    var open = document.querySelector("details[data-menu][open] summary");
    closeMenus(null);
    if (open) { open.focus(); }
  });

  // Choosing several accounts at once. The count is shown rather than
  // implied, and both buttons stay disabled until something is ticked:
  // a bulk restore that silently did nothing would be read as one that
  // silently did something.
  Array.prototype.forEach.call(document.querySelectorAll("form[data-bulk]"), function (form) {
    var boxes = Array.prototype.slice.call(form.querySelectorAll('input[name="account"]'));
    var all = form.querySelector("[data-check-all]");
    var count = form.querySelector("[data-bulk-count]");
    var submits = Array.prototype.slice.call(form.querySelectorAll("[data-bulk-submit]"));
    if (boxes.length === 0) { return; }

    function refresh() {
      var chosen = boxes.filter(function (box) { return box.checked; }).length;
      if (count) {
        count.textContent = chosen === 0
          ? "Nothing chosen"
          : chosen + " of " + boxes.length + " chosen";
      }
      submits.forEach(function (button) { button.disabled = chosen === 0; });
      if (all) {
        all.checked = chosen === boxes.length;
        all.indeterminate = chosen > 0 && chosen < boxes.length;
      }
    }

    boxes.forEach(function (box) { box.addEventListener("change", refresh); });
    if (all) {
      all.addEventListener("change", function () {
        boxes.forEach(function (box) { box.checked = all.checked; });
        refresh();
      });
    }
    refresh();
  });

})();

// Pagination.
//
// Nineteen accounts a night fills a table faster than anyone reads it, and
// a long page is slow to scroll and slower to scan. The rows are already
// here — the server sends a bounded number of them — so the paging happens
// in the browser, which keeps it instant and keeps it working with the sort
// and the filter beside it.
//
// The control appears only when there is something to page through: a table
// shorter than the smallest page size gets no furniture it does not need.
(function () {
  "use strict";

  var SIZES = [20, 50, 100];
  var STORAGE = "gniza.rows";
  var states = [];

  function preferred() {
    try {
      var saved = parseInt(window.localStorage.getItem(STORAGE), 10);
      if (SIZES.indexOf(saved) !== -1) { return saved; }
    } catch (e) {}
    return SIZES[0];
  }

  function remember(size) {
    try { window.localStorage.setItem(STORAGE, String(size)); } catch (e) {}
  }

  function eligible(table) {
    var body = table.tBodies[0];
    if (!body) { return []; }
    return Array.prototype.slice.call(body.rows).filter(function (row) {
      // A row the filter has already rejected is not on any page.
      return row.dataset.filtered !== "1";
    });
  }

  function draw(state) {
    var rows = eligible(state.table);
    var pages = Math.max(1, Math.ceil(rows.length / state.size));
    if (state.page > pages) { state.page = pages; }
    var first = (state.page - 1) * state.size;
    var last = Math.min(first + state.size, rows.length);

    rows.forEach(function (row, index) {
      row.hidden = index < first || index >= last;
    });

    // Nothing to page through: the control would be noise.
    state.bar.hidden = rows.length <= SIZES[0];
    state.range.textContent = rows.length === 0
      ? "nothing to show"
      : (first + 1) + "–" + last + " of " + rows.length;
    state.previous.disabled = state.page <= 1;
    state.next.disabled = state.page >= pages;
  }

  function build(table) {
    var wrap = table.closest("[data-tablewrap]") || table;

    var bar = document.createElement("div");
    bar.className = "cpr:flex cpr:flex-wrap cpr:items-center cpr:gap-3 cpr:px-4 cpr:py-2.5 cpr:border-t cpr:border-base-300 cpr:text-[13px] cpr:text-base-content/70";

    var label = document.createElement("label");
    label.className = "cpr:flex cpr:items-center cpr:gap-1.5 cpr:font-semibold";
    label.textContent = "Rows ";
    var select = document.createElement("select");
    select.className = "cpr:select cpr:select-sm cpr:w-auto";
    SIZES.forEach(function (size) {
      var option = document.createElement("option");
      option.value = String(size);
      option.textContent = String(size);
      select.appendChild(option);
    });
    label.appendChild(select);

    var range = document.createElement("span");
    range.className = "cpr:tabular-nums";
    range.setAttribute("aria-live", "polite");

    var previous = document.createElement("button");
    previous.type = "button";
    previous.className = "cpr:btn cpr:btn-sm cpr:btn-ghost";
    previous.textContent = "Previous";
    var next = document.createElement("button");
    next.type = "button";
    next.className = "cpr:btn cpr:btn-sm cpr:btn-ghost";
    next.textContent = "Next";

    var buttons = document.createElement("div");
    buttons.className = "cpr:flex cpr:gap-1.5 cpr:ml-auto";
    buttons.appendChild(previous);
    buttons.appendChild(next);

    bar.appendChild(label);
    bar.appendChild(range);
    bar.appendChild(buttons);
    wrap.parentNode.insertBefore(bar, wrap.nextSibling);

    var state = {
      table: table, bar: bar, range: range, select: select,
      previous: previous, next: next, size: preferred(), page: 1,
    };
    select.value = String(state.size);

    select.addEventListener("change", function () {
      state.size = parseInt(select.value, 10) || SIZES[0];
      state.page = 1;
      remember(state.size);
      // One choice, every table: an operator who wants a hundred rows
      // wants them on the next page too.
      states.forEach(function (other) {
        if (other !== state) {
          other.size = state.size;
          other.page = 1;
          other.select.value = String(state.size);
          draw(other);
        }
      });
      draw(state);
    });
    previous.addEventListener("click", function () {
      if (state.page > 1) { state.page--; draw(state); }
    });
    next.addEventListener("click", function () {
      state.page++;
      draw(state);
    });

    states.push(state);
    draw(state);
  }

  Array.prototype.forEach.call(
    document.querySelectorAll("table[data-sortable], table[data-paginate]"), build);

  // Sorting reorders the rows under the page, and a live refresh replaces
  // them; both leave the page showing whatever landed in the slice.
  window.gnizaRepaginate = function (table, toFirstPage) {
    states.forEach(function (state) {
      if (table && state.table !== table) { return; }
      if (toFirstPage) { state.page = 1; }
      draw(state);
    });
  };
})();

// Sortable tables.
//
// The server sends every table in the order that matters most — newest
// first — and that is the order an operator wants nine times out of ten.
// The tenth time they are asking a different question of the same rows:
// which account stored the most, which run failed, who has not been backed
// up. Doing that server-side would mean a round trip and a lost scroll
// position for a question answered from rows already on the page.
//
// What a cell shows and what it sorts by are not the same: "2 h ago" and
// "6.2 MiB new of 152.5 MiB" sort as text into nonsense, so those cells
// carry data-sort with a number in it. Anything without one sorts on its
// text, which is right for an account name.
(function () {
  "use strict";

  // Tables an operator has reordered, so a live refresh can put their
  // order back over the rows the server just sent.
  var chosen = [];

  function key(row, column) {
    var cell = row.children[column];
    if (!cell) { return ""; }
    var raw = cell.getAttribute("data-sort");
    return raw === null ? cell.textContent.trim() : raw;
  }

  function numeric(rows, column) {
    for (var i = 0; i < rows.length; i++) {
      var value = key(rows[i], column);
      if (value !== "" && isNaN(Number(value))) { return false; }
    }
    return rows.length > 0;
  }

  function apply(table, column, descending) {
    var body = table.tBodies[0];
    if (!body) { return; }
    var rows = Array.prototype.slice.call(body.rows);
    var asNumber = numeric(rows, column);
    rows.sort(function (a, b) {
      var left = key(a, column);
      var right = key(b, column);
      var order;
      if (asNumber) {
        order = Number(left) - Number(right);
      } else {
        order = left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
      }
      return descending ? -order : order;
    });
    rows.forEach(function (row) { body.appendChild(row); });

    Array.prototype.forEach.call(table.tHead.rows[0].cells, function (cell, index) {
      if (index === column) {
        cell.setAttribute("aria-sort", descending ? "descending" : "ascending");
      } else if (cell.hasAttribute("aria-sort")) {
        cell.setAttribute("aria-sort", "none");
      }
    });

    // The rows moved, so which of them are on this page did too. Sorting
    // is a new question, and the answer starts at the top of it.
    if (typeof window.gnizaRepaginate === "function") {
      window.gnizaRepaginate(table, true);
    }
  }

  function remember(table, column, descending) {
    for (var i = 0; i < chosen.length; i++) {
      if (chosen[i].table === table) {
        chosen[i].column = column;
        chosen[i].descending = descending;
        return;
      }
    }
    chosen.push({ table: table, column: column, descending: descending });
  }

  function prepare(table) {
    if (!table.tHead || !table.tHead.rows.length) { return; }
    Array.prototype.forEach.call(table.tHead.rows[0].cells, function (cell, index) {
      if (cell.hasAttribute("data-nosort") || cell.textContent.trim() === "") { return; }
      var label = cell.textContent.trim();
      var button = document.createElement("button");
      button.type = "button";
      button.className = "cpr:sortbtn";
      button.textContent = label;
      cell.textContent = "";
      cell.appendChild(button);
      cell.setAttribute("aria-sort", "none");
      button.addEventListener("click", function () {
        // Same column again reverses it; a new column starts descending
        // for numbers and dates, ascending for names, which is what each
        // one is usually being asked for.
        var current = cell.getAttribute("aria-sort");
        var descending = current === "none"
          ? numeric(Array.prototype.slice.call(table.tBodies[0].rows), index)
          : current === "ascending";
        apply(table, index, descending);
        remember(table, index, descending);
      });
    });
  }

  Array.prototype.forEach.call(document.querySelectorAll("table[data-sortable]"), prepare);

  // Live refresh replaces the rows wholesale. Without this the operator's
  // chosen order silently reverts to the server's every three seconds.
  window.gnizaReapplySort = function () {
    chosen.forEach(function (state) {
      if (document.contains(state.table)) {
        apply(state.table, state.column, state.descending);
      }
    });
  };
})();

// While work is in flight, keep the page current without reloading it.
//
// A full reload threw away the scroll position and any open report, which
// on a nineteen-account run meant the page jumped to the top every few
// seconds. Instead the same page is fetched, parsed here, and only the
// regions marked data-live are swapped. The response is HTML we already
// know how to render, so nothing depends on cpsrvd passing a content type
// through, which it does not.
(function () {
  "use strict";
  var INTERVAL = 3000;
  var timer = null;
  var inFlight = false;

  function running() { return document.querySelector("[data-running]") !== null; }

  function swap(fresh) {
    var swapped = false;
    // A fold the operator opened is theirs until they close it. The
    // markup is replaced every three seconds, so which ones were open is
    // remembered across the replacement -- otherwise reading the list of
    // waiting accounts is impossible: it shuts under the cursor.
    var open = {};
    document.querySelectorAll("details[data-fold][open]").forEach(function (fold) {
      open[fold.dataset.fold] = true;
    });
    Array.prototype.forEach.call(fresh.querySelectorAll("[data-live]"), function (node) {
      var here = document.querySelector('[data-live="' + node.dataset.live + '"]');
      if (here && here.innerHTML !== node.innerHTML) {
        // A region asking to stay at its end is the service log: it is
        // read at the newest line, and a swap that left the scroll where
        // it was would show the oldest one every few seconds.
        var atEnd = here.dataset.liveScroll === "end";
        here.innerHTML = node.innerHTML;
        if (atEnd) { here.scrollTop = here.scrollHeight; }
        here.querySelectorAll("details[data-fold]").forEach(function (fold) {
          if (open[fold.dataset.fold]) { fold.open = true; }
        });
        swapped = true;
      }
    });
    if (swapped && typeof window.gnizaReapplyFilters === "function") {
      // The rows are new, so the filter the operator typed has to be put
      // back over them.
      window.gnizaReapplyFilters();
    }
    if (swapped && typeof window.gnizaReapplySort === "function") {
      window.gnizaReapplySort();
    }
    if (swapped && typeof window.gnizaRepaginate === "function") {
      // Stay on the page the operator was reading rather than snapping
      // back to the first one every three seconds.
      window.gnizaRepaginate(null, false);
    }
  }

  function tick() {
    timer = null;
    if (inFlight) { return; }
    // A report the operator has open must not be pulled out from under
    // them; the next tick will catch up.
    if (document.querySelector("dialog[open]")) { schedule(); return; }

    inFlight = true;
    // Through DirectAdmin's Evolution skin the address bar is the skin's,
    // and fetching it answers with the skin; the page says where it can
    // be fetched from instead.
    var self = document.querySelector('meta[name="gniza-live"]');
    window.fetch(self && self.content ? self.content : window.location.href, {
      credentials: "same-origin",
      headers: { "X-Gniza-Live": "1" }
    }).then(function (response) {
      return response.ok ? response.text() : null;
    }).then(function (html) {
      if (!html) { return; }
      var fresh = new DOMParser().parseFromString(html, "text/html");
      swap(fresh);
    }).catch(function () {
      // A refresh that fails is not worth reporting: the page still shows
      // what it last knew, and the next tick tries again.
    }).then(function () {
      inFlight = false;
      schedule();
    });
  }

  function schedule() {
    if (timer !== null || !running()) { return; }
    timer = window.setTimeout(tick, INTERVAL);
  }

  // Open the log at its end for the same reason, whether or not anything
  // is being followed.
  document.querySelectorAll('[data-live-scroll="end"]').forEach(function (box) {
    box.scrollTop = box.scrollHeight;
  });

  schedule();
})();

// A report is long and wanted occasionally, so it lives in a dialog rather
// than under every row.
(function () {
  "use strict";
  // What the drawer holds when nothing has been loaded into it: the form
  // for adding a destination. Editing borrows the same drawer and puts
  // this back on the way out.
  var drawerBody = document.querySelector("[data-drawer-body]");
  var drawerTitle = document.querySelector("[data-drawer-title]");
  var drawerHome = drawerBody ? { html: drawerBody.innerHTML, title: drawerTitle.textContent } : null;

  function focusFirst(dialog) {
    var first = dialog.querySelector("input:not([type=hidden]), select, textarea");
    if (first) { first.focus(); }
  }

  // Editing loads the form for that destination into the drawer. It is
  // fetched as the page it already is, and read here — the same trick the
  // live refresh uses, and for the same reason: nothing then depends on
  // cpsrvd passing a content type through, which it does not.
  function loadIntoDrawer(dialog, link) {
    drawerTitle.textContent = link.dataset.dialogTitle || drawerHome.title;
    drawerBody.innerHTML = '<p class="cpr:hint">Loading…</p>';
    dialog.showModal();

    window.fetch(link.href, { credentials: "same-origin" })
      .then(function (response) { return response.ok ? response.text() : null; })
      .then(function (html) {
        if (!html) { throw new Error("no page"); }
        var fetched = new DOMParser().parseFromString(html, "text/html");
        var content = fetched.querySelector("[data-drawer-content]");
        if (!content) { throw new Error("no form"); }
        drawerBody.innerHTML = content.innerHTML;
        document.dispatchEvent(new CustomEvent("gniza:loaded", { detail: dialog }));
        focusFirst(dialog);
      })
      .catch(function () {
        // The form exists as its own page; if it cannot be brought here,
        // go to it rather than leaving an empty drawer.
        window.location.href = link.href;
      });
  }

  document.addEventListener("click", function (event) {
    var opener = event.target.closest("[data-dialog]");
    if (opener) {
      var dialog = document.getElementById(opener.dataset.dialog);
      if (dialog && dialog.showModal) {
        // Only then: without this the link still goes to the same form on
        // a page of its own, which is what a browser with no JavaScript
        // gets and what a shared link opens.
        event.preventDefault();
        if (opener.hasAttribute("data-dialog-fetch") && drawerBody) {
          loadIntoDrawer(dialog, opener);
          return;
        }
        dialog.showModal();
        focusFirst(dialog);
      }
      return;
    }
    // Clicking the backdrop is how a sheet is dismissed by everyone who
    // has ever used one.
    if (event.target.tagName === "DIALOG" && event.target.hasAttribute("data-sheet")) {
      event.target.close();
      return;
    }
    var closer = event.target.closest("[data-dialog-close]");
    if (closer) {
      var open = closer.closest("dialog");
      if (open) { open.close(); }
    }
  });

  // A form that came back with something wrong reopens where it was being
  // filled in, rather than leaving the reason on a page behind it.
  var refused = document.querySelector("dialog[data-sheet][data-drawer-open]");
  if (refused && refused.showModal) {
    refused.showModal();
    focusFirst(refused);
  }

  // Whatever was loaded in goes away with the drawer, so opening it again
  // is opening the same thing it was before.
  var drawer = document.querySelector("dialog[data-sheet]");
  if (drawer && drawerHome) {
    drawer.addEventListener("close", function () {
      drawerBody.innerHTML = drawerHome.html;
      drawerTitle.textContent = drawerHome.title;
    });
  }
})();

// Theme control. WHM does not tell a plugin which theme it is wearing, so
// the operator gets the final say. Light is the default — it is the theme
// this interface is designed in — and an operator who prefers dark, or who
// wants the machine's preference to decide, says so here. The choice lives
// in this browser only: it is a viewing preference, not server state.
(function () {
  "use strict";
  var group = document.getElementById("theme");
  if (!group) { return; }

  function stored() {
    var choice;
    try { choice = localStorage.getItem("gniza.theme"); } catch (e) { return "light"; }
    return (choice === "dark" || choice === "system") ? choice : "light";
  }

  function apply(choice) {
    if (choice === "light" || choice === "dark") {
      document.documentElement.setAttribute("data-theme", choice);
    } else {
      document.documentElement.removeAttribute("data-theme");
    }
    Array.prototype.forEach.call(group.querySelectorAll("button"), function (button) {
      button.setAttribute("aria-pressed", String(button.dataset.themeChoice === choice));
    });
    try { localStorage.setItem("gniza.theme", choice); } catch (e) {}
  }

  // Only offered when it can actually work.
  group.hidden = false;
  apply(stored());

  group.addEventListener("click", function (event) {
    var button = event.target.closest("[data-theme-choice]");
    if (button) { apply(button.dataset.themeChoice); }
  });
})();

// Select-all for the pickers. A backup of an account with forty mailboxes
// is a page where "all of them" costs forty clicks.
//
// The box ships hidden and is shown here, because without scripting it
// could not tick anything and a control that does nothing is worse than no
// control: every picker already means all of them when none is ticked.
(function () {
  Array.prototype.forEach.call(document.querySelectorAll("[data-checkall]"), function (master) {
    var table = master.closest("table");
    if (!table) { return; }

    function boxes() {
      return Array.prototype.slice.call(
        table.querySelectorAll('tbody input[type="checkbox"][name="name"]'));
    }
    function sync() {
      var all = boxes();
      var ticked = all.filter(function (box) { return box.checked; });
      master.checked = all.length > 0 && ticked.length === all.length;
      // Partly ticked is its own state, and showing it as unticked would
      // make one click clear a selection somebody just made.
      master.indeterminate = ticked.length > 0 && ticked.length < all.length;
    }

    master.hidden = false;
    master.addEventListener("change", function () {
      boxes().forEach(function (box) { box.checked = master.checked; });
      sync();
    });
    table.addEventListener("change", function (event) {
      if (event.target !== master) { sync(); }
    });
    sync();
  });
})();

// A public key is a line somebody has to get into another server's
// authorized_keys, and selecting sixty characters of base64 by hand is how
// a key ends up pasted with half of it missing. The button is added by
// script and the key is selectable text without it, so nothing is lost
// where there is no clipboard to write to.
(function () {
  function copy(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    // Older browsers, and any page not served over https.
    return new Promise(function (resolve, reject) {
      var box = document.createElement("textarea");
      box.value = text;
      box.setAttribute("readonly", "");
      box.style.position = "fixed";
      box.style.opacity = "0";
      document.body.appendChild(box);
      box.select();
      var ok = false;
      try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
      document.body.removeChild(box);
      ok ? resolve() : reject(new Error("copy refused"));
    });
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("[data-copy]") : null;
    if (!button) { return; }
    var source = document.querySelector(button.getAttribute("data-copy"));
    if (!source) { return; }

    var said = button.textContent;
    copy(source.textContent.trim()).then(function () {
      button.textContent = "Copied";
    }, function () {
      // Say so rather than leaving somebody to paste what they think they
      // copied. The key is on the page; selecting it still works.
      button.textContent = "Select it and copy";
    });
    window.setTimeout(function () { button.textContent = said; }, 3000);
  });
})();
