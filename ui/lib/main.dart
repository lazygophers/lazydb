// lazydb 桌面界面（#20）：左连接列表、中结构树（懒加载）、右 SQL + 结果。
// 界面零业务逻辑：所有数据经 Backend 的 HTTP API（ADR-0001）。
import 'package:file_selector/file_selector.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:re_editor/re_editor.dart';

import 'backend.dart';
import 'result_grid.dart';
import 'sql_editor.dart';

void main() {
  runApp(const LazyDbApp());
}

class LazyDbApp extends StatelessWidget {
  const LazyDbApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'lazydb',
      theme: ThemeData(
          useMaterial3: true,
          colorScheme: ColorScheme.fromSeed(
              seedColor: const Color(0xFF4F8CC9), brightness: Brightness.dark)),
      home: const HomePage(),
    );
  }
}

// ---- 结构树节点（纯 UI 状态，展开即经 API 懒加载子层） ----

class TreeNode {
  TreeNode(this.label, this.kind, this.path);
  final String label;
  final String kind; // conn | database | table | view | cols | idx | ddl | leaf | error
  final List<String> path; // API 的 path 参数
  bool expanded = false;
  bool loaded = false;
  bool loading = false;
  List<TreeNode> children = [];
}

class HomePage extends StatefulWidget {
  const HomePage({super.key});

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  Backend? be;
  String? bootError;
  List<dynamic> conns = [];
  final trees = <String, List<TreeNode>>{}; // connID → 根层节点
  String? selectedConn;

  QueryResult? result;
  String runError = '';
  bool running = false;
  String? ddlText;
  final sql = CodeLineEditingController.fromText('');
  final words = SqlWords();
  final tableDb = <String, String>{}; // 表名 → 库名（补全取字段用）

  // 结构搜索（#23）：非空时结构树换成搜索结果。
  final searchCtl = TextEditingController();
  List<dynamic>? searchHits;
  bool exporting = false;

  @override
  void dispose() {
    sql.dispose();
    searchCtl.dispose();
    super.dispose();
  }

  @override
  void initState() {
    super.initState();
    words.onTableMiss = _fetchColumns;
    _boot();
  }

  Future<void> _boot() async {
    setState(() {
      be = null;
      bootError = null;
    });
    try {
      final b = await Backend.attach(Backend.findBinary());
      setState(() => be = b);
      await _reload();
    } catch (e) {
      setState(() => bootError = '$e');
    }
  }

  Future<void> _reload() async {
    final list = await be!.listConnections();
    setState(() => conns = list);
    final ids = list.map((c) => c['id'] as String).toSet();
    trees.removeWhere((id, _) => !ids.contains(id));
  }

  // ---- 连接 ----

  Future<void> _connDialog({Map<String, dynamic>? old, String? oldId}) async {
    final name = TextEditingController(text: old?['name'] ?? '');
    final dsn =
        TextEditingController(text: old?['config']?['dsn'] ?? '');
    var driver = old?['config']?['driver'] ?? 'sqlite';
    String testMsg = '';
    await showDialog(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialog) => AlertDialog(
          title: Text(oldId == null ? '新建连接' : '编辑连接'),
          content: SizedBox(
            width: 460,
            child: Column(mainAxisSize: MainAxisSize.min, children: [
              TextField(
                  controller: name,
                  decoration: const InputDecoration(labelText: '名称')),
              const SizedBox(height: 8),
              DropdownButtonFormField<String>(
                initialValue: driver,
                items: const [
                  DropdownMenuItem(value: 'sqlite', child: Text('SQLite')),
                  DropdownMenuItem(value: 'mysql', child: Text('MySQL')),
                ],
                onChanged: (v) => setDialog(() => driver = v!),
              ),
              const SizedBox(height: 8),
              TextField(
                  controller: dsn,
                  decoration: const InputDecoration(
                      labelText: 'DSN（sqlite 填文件路径，mysql 填连接串）')),
              const SizedBox(height: 8),
              Text(testMsg, style: const TextStyle(fontSize: 12)),
            ]),
          ),
          actions: [
            TextButton(
              onPressed: () async {
                setDialog(() => testMsg = '测试中…');
                try {
                  final r = await be!.testConnection(
                      {'driver': driver, 'dsn': dsn.text});
                  setDialog(() =>
                      testMsg = r['ok'] == true ? '连通 ✓' : '不通：${r['error']}');
                } catch (e) {
                  setDialog(() => testMsg = '不通：$e');
                }
              },
              child: const Text('测试连通'),
            ),
            TextButton(
                onPressed: () => Navigator.pop(context), child: const Text('取消')),
            FilledButton(
              onPressed: () async {
                try {
                  if (oldId != null) {
                    await be!.deleteConnection(oldId);
                    trees.remove(oldId);
                  }
                  await be!.createConnection(
                      name.text, {'driver': driver, 'dsn': dsn.text});
                  await _reload();
                  if (context.mounted) Navigator.pop(context);
                } catch (e) {
                  setDialog(() => testMsg = '保存失败：$e');
                }
              },
              child: const Text('保存'),
            ),
          ],
        ),
      ),
    );
  }

  Future<void> _deleteConn(String id) async {
    await be!.deleteConnection(id);
    trees.remove(id);
    if (selectedConn == id) setState(() => selectedConn = null);
    await _reload();
  }

  // ---- 结构树懒加载 ----

  Future<void> _toggle(String connId, TreeNode n) async {
    if (n.kind == 'ddl') {
      await _showDdl(connId, n);
      return;
    }
    n.expanded = !n.expanded;
    setState(() {});
    if (!n.expanded || n.loaded || n.loading) return;
    n.loading = true;
    setState(() {});
    try {
      final kids = <TreeNode>[];
      switch (n.kind) {
        case 'conn':
          for (final node in await be!.children(connId, [])) {
            kids.add(TreeNode(node['name'], node['kind'], [node['name']]));
          }
        case 'database':
          for (final node in await be!.children(connId, n.path)) {
            final t = TreeNode(node['name'], node['kind'], [...n.path, node['name']]);
            kids.add(t);
          }
        case 'table':
        case 'view':
          kids.addAll([
            TreeNode('字段', 'cols', n.path),
            TreeNode('索引', 'idx', n.path),
            TreeNode('DDL', 'ddl', n.path),
          ]);
        case 'cols':
          final r = await be!.columns(connId, n.path);
          final cols = (r['columns'] as List).cast<Map<String, dynamic>>();
          words.setColumns(n.path.last, cols); // 树里看过的表直接进补全词
          for (final c in cols) {
            final nullTxt = c['nullable'] == true ? '' : ' NOT NULL';
            kids.add(TreeNode(
                '${c['name']}  ${c['type']}$nullTxt', 'leaf', n.path));
          }
        case 'idx':
          final r = await be!.indexes(connId, n.path);
          for (final i in r['indexes']) {
            final u = i['unique'] == true ? 'UNIQUE ' : '';
            kids.add(TreeNode(
                '$u${i['name']} (${(i['columns'] as List).join(', ')})',
                'leaf',
                n.path));
          }
        default:
          break;
      }
      n.children = kids;
      n.loaded = true;
    } catch (e) {
      n.children = [TreeNode('$e', 'error', n.path)];
    } finally {
      n.loading = false;
      setState(() {});
    }
  }

  Future<void> _showDdl(String connId, TreeNode n) async {
    setState(() {
      ddlText = '加载中…';
      result = null;
    });
    try {
      final ddl = await be!.ddl(connId, n.path);
      setState(() => ddlText = ddl);
    } catch (e) {
      setState(() => ddlText = '$e');
    }
  }

  Future<void> _runSql() async {
    if (selectedConn == null) return;
    final text = sql.selectedText.trim().isNotEmpty ? sql.selectedText : sql.text;
    if (text.trim().isEmpty) return;
    setState(() {
      running = true;
      runError = '';
      ddlText = null;
    });
    try {
      result = QueryResult.fromJson(
          await be!.exec(selectedConn!, text) as Map<String, dynamic>);
    } catch (e) {
      result = null;
      runError = '$e';
    } finally {
      setState(() => running = false);
    }
  }

  // 导出当前语句结果（#24）：存盘面板选位置，后端流式下载。
  Future<void> _export(String format) async {
    if (selectedConn == null || running) return;
    final text = sql.selectedText.trim().isNotEmpty ? sql.selectedText : sql.text;
    if (text.trim().isEmpty) return;
    final loc = await getSaveLocation(
        suggestedName: 'export.$format',
        acceptedTypeGroups: [
          XTypeGroup(label: format.toUpperCase(), extensions: [format])
        ]);
    if (loc == null) return;
    setState(() => exporting = true);
    try {
      await be!.exportToFile(selectedConn!, text, format, loc.path);
    } catch (e) {
      setState(() => runError = '导出失败：$e');
    } finally {
      setState(() => exporting = false);
    }
  }

  // ---- 补全供词（全走缓存接口，断网时后端 stale-fallback 仍供词） ----

  Future<void> _loadWords() async {
    if (selectedConn == null) return;
    try {
      final tables = <String>[];
      tableDb.clear();
      for (final db in await be!.children(selectedConn!, [])) {
        for (final t in await be!.children(selectedConn!, [db['name']])) {
          tables.add('${t['name']}');
          tableDb['${t['name']}'] = '${db['name']}';
        }
      }
      words.setTables(tables);
    } catch (_) {/* 供词失败不拦主流程 */}
  }

  Future<void> _fetchColumns(String table) async {
    final db = tableDb[table];
    if (db == null) return;
    try {
      final r = await be!.columns(selectedConn!, [db, table]);
      words.setColumns(table, (r['columns'] as List).cast<Map<String, dynamic>>());
    } catch (_) {/* 同上 */}
  }

  // ---- 结构搜索（#23，全走后端内存索引） ----

  Future<void> _search(String q) async {
    if (selectedConn == null || q.isEmpty) {
      setState(() => searchHits = null);
      return;
    }
    try {
      final hits = await be!.search(selectedConn!, q);
      if (searchCtl.text == q) setState(() => searchHits = hits);
    } catch (_) {
      setState(() => searchHits = const []);
    }
  }

  // ---- 查询历史（#23） ----

  Future<void> _historyDialog() async {
    final q = TextEditingController();
    await showDialog(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialog) {
          Future<List<dynamic>> items() => be!.history(q.text);
          return AlertDialog(
            title: const Text('查询历史'),
            content: SizedBox(
              width: 640,
              height: 420,
              child: Column(children: [
                TextField(
                  controller: q,
                  decoration: const InputDecoration(
                      hintText: '按语句/连接/报错关键字搜',
                      isDense: true,
                      prefixIcon: Icon(Icons.search, size: 18)),
                  onChanged: (_) => setDialog(() {}),
                ),
                const SizedBox(height: 8),
                Expanded(
                  child: FutureBuilder<List<dynamic>>(
                    future: items(),
                    builder: (context, snap) {
                      if (!snap.hasData) {
                        return const Center(
                            child: SizedBox(
                                width: 18,
                                height: 18,
                                child: CircularProgressIndicator(strokeWidth: 2)));
                      }
                      if (snap.data!.isEmpty) {
                        return const Center(
                            child: Text('无记录', style: TextStyle(color: Colors.grey)));
                      }
                      return ListView.builder(
                        itemCount: snap.data!.length,
                        itemBuilder: (context, i) {
                          final e = snap.data![i] as Map<String, dynamic>;
                          final dt = DateTime.fromMillisecondsSinceEpoch(e['ts']);
                          return ListTile(
                            dense: true,
                            title: Text('${e['sql']}',
                                maxLines: 1,
                                overflow: TextOverflow.ellipsis,
                                style: const TextStyle(fontFamily: 'monospace', fontSize: 12)),
                            subtitle: Text(
                                '${dt.toLocal()}  连接 ${e['conn']}  ${e['ms']}ms  ${e['ok'] == true ? '成功' : '失败：${e['error'] ?? ''}'}',
                                style: const TextStyle(fontSize: 10)),
                            onTap: () {
                              sql.text = e['sql'] as String; // 点历史语句取回编辑器
                              Navigator.pop(context);
                            },
                          );
                        },
                      );
                    },
                  ),
                ),
              ]),
            ),
            actions: [
              TextButton(
                  onPressed: () => Navigator.pop(context),
                  child: const Text('关闭')),
            ],
          );
        },
      ),
    );
    q.dispose();
  }

  // ---- 布局 ----

  @override
  Widget build(BuildContext context) {
    if (bootError != null) {
      return Scaffold(
        body: Center(
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Text('后端不可用：$bootError'),
            const SizedBox(height: 8),
            FilledButton(onPressed: _boot, child: const Text('重试')),
          ]),
        ),
      );
    }
    if (be == null) {
      return const Scaffold(body: Center(child: Text('连接后端…')));
    }
    return Scaffold(
      body: Row(children: [
        SizedBox(width: 230, child: _connsPane()),
        const VerticalDivider(width: 1),
        SizedBox(width: 300, child: _treePane()),
        const VerticalDivider(width: 1),
        Expanded(child: _queryPane()),
      ]),
    );
  }

  Widget _connsPane() {
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Padding(
        padding: const EdgeInsets.all(8),
        child: Row(children: [
          const Text('连接'),
          const Spacer(),
          IconButton(
              tooltip: '新建连接',
              icon: const Icon(Icons.add),
              onPressed: () => _connDialog()),
        ]),
      ),
      Expanded(
        child: ListView.builder(
          itemCount: conns.length,
          itemBuilder: (context, i) {
            final c = conns[i] as Map<String, dynamic>;
            final id = c['id'] as String;
            return ListTile(
              dense: true,
              selected: selectedConn == id,
              title: Text('${c['name']}'),
              subtitle: Text('${c['config']['driver']}',
                  style: const TextStyle(fontSize: 11)),
              onTap: () {
                setState(() => selectedConn = id);
                _loadWords();
              },
              trailing: PopupMenuButton<String>(
                onSelected: (op) => op == 'edit'
                    ? _connDialog(old: c, oldId: id)
                    : _deleteConn(id),
                itemBuilder: (_) => const [
                  PopupMenuItem(value: 'edit', child: Text('编辑')),
                  PopupMenuItem(value: 'delete', child: Text('删除')),
                ],
              ),
            );
          },
        ),
      ),
    ]);
  }

  Widget _treePane() {
    if (selectedConn == null) {
      return const Center(
          child: Text('先选一个连接', style: TextStyle(color: Colors.grey)));
    }
    final roots = trees.putIfAbsent(selectedConn!, () => []);
    if (roots.isEmpty) {
      // 根 = 连接自身
      final c = conns.firstWhere((x) => x['id'] == selectedConn)
          as Map<String, dynamic>;
      roots.add(TreeNode('${c['name']}', 'conn', []));
    }
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Padding(
        padding: const EdgeInsets.fromLTRB(8, 8, 8, 4),
        child: TextField(
          controller: searchCtl,
          decoration: const InputDecoration(
              hintText: '搜索表/字段', isDense: true, prefixIcon: Icon(Icons.search, size: 18)),
          onChanged: _search,
        ),
      ),
      Padding(
        padding: const EdgeInsets.fromLTRB(8, 0, 8, 4),
        child: Row(children: [
          const Text('结构'),
          const Spacer(),
          IconButton(
              tooltip: '刷新缓存',
              icon: const Icon(Icons.refresh),
              onPressed: () async {
                await be!.refresh(selectedConn!, []);
                trees.remove(selectedConn);
                setState(() {});
              }),
        ]),
      ),
      Expanded(
        child: searchCtl.text.isNotEmpty
            ? _searchList()
            : ListView(
                children: [for (final n in roots) _treeNode(selectedConn!, n, 0)]),
      ),
    ]);
  }

  // 搜索结果：kind 图标 + db.table（字段名后缀列名）。
  Widget _searchList() {
    final hits = searchHits;
    if (hits == null) {
      return const Center(child: SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2)));
    }
    if (hits.isEmpty) {
      return const Center(child: Text('无匹配', style: TextStyle(color: Colors.grey)));
    }
    return ListView.builder(
      itemCount: hits.length,
      itemBuilder: (context, i) {
        final m = hits[i] as Map<String, dynamic>;
        final col = m['column'] as String?;
        return ListTile(
          dense: true,
          leading: Icon(m['kind'] == 'table' ? Icons.table_chart : Icons.view_column_outlined,
              size: 16, color: Colors.grey),
          title: Text(col == null ? '${m['table']}' : '${m['table']}.$col',
              style: const TextStyle(fontFamily: 'monospace', fontSize: 12)),
          subtitle: col == null
              ? null
              : Text('${m['type'] ?? ''}', style: const TextStyle(fontSize: 10)),
          onTap: () {
            // 点表名把它带进编辑器，省一次手打
            final name = (m['table'] as String).split('.');
            sql.text = 'SELECT *\nFROM ${name.length == 2 ? name[1] : m['table']};';
          },
        );
      },
    );
  }

  Widget _treeNode(String connId, TreeNode n, int depth) {
    final expandable = switch (n.kind) {
      'conn' || 'database' || 'table' || 'view' || 'cols' || 'idx' => true,
      _ => false,
    };
    return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
      InkWell(
        onTap: expandable || n.kind == 'ddl' ? () => _toggle(connId, n) : null,
        child: Padding(
          padding: EdgeInsets.only(left: 8.0 + depth * 14, top: 3, bottom: 3),
          child: Row(children: [
            if (expandable)
              Icon(
                n.expanded
                    ? Icons.keyboard_arrow_down
                    : Icons.keyboard_arrow_right,
                size: 16,
                color: Colors.grey,
              )
            else
              const SizedBox(width: 16),
            const SizedBox(width: 2),
            Flexible(
                child: Text(n.label,
                    overflow: TextOverflow.ellipsis,
                    style: TextStyle(
                        fontSize: 12,
                        color: n.kind == 'error' ? Colors.redAccent : null,
                        fontFamily:
                            n.kind == 'leaf' || n.kind == 'error' ? 'monospace' : null))),
            if (n.loading)
              const Padding(
                padding: EdgeInsets.only(left: 6),
                child: SizedBox(
                    width: 10,
                    height: 10,
                    child: CircularProgressIndicator(strokeWidth: 2)),
              ),
          ]),
        ),
      ),
      if (n.expanded)
        Padding(
          padding: const EdgeInsets.only(left: 4),
          child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                for (final c in n.children) _treeNode(connId, c, depth + 1)
              ]),
        ),
    ]);
  }

  Widget _queryPane() {
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      SizedBox(
        height: 220,
        child: CallbackShortcuts(
          bindings: {
            const SingleActivator(LogicalKeyboardKey.enter, meta: true): _runSql,
            const SingleActivator(LogicalKeyboardKey.enter, control: true):
                _runSql,
          },
          child: SqlEditor(controller: sql, words: words),
        ),
      ),
      Padding(
        padding: const EdgeInsets.all(8),
        child: Row(children: [
          Text(selectedConn == null ? '未选连接' : '整段执行；有选中则只执行选中',
              style: const TextStyle(fontSize: 11, color: Colors.grey)),
          const Spacer(),
          IconButton(
              tooltip: '查询历史',
              icon: const Icon(Icons.history, size: 18),
              onPressed: _historyDialog),
          const SizedBox(width: 8),
          PopupMenuButton<String>(
            tooltip: '导出',
            enabled: selectedConn != null && !exporting && !running,
            onSelected: _export,
            itemBuilder: (_) => const [
              PopupMenuItem(value: 'csv', child: Text('导出 CSV')),
              PopupMenuItem(value: 'xlsx', child: Text('导出 Excel')),
            ],
          ),
          const SizedBox(width: 8),
          FilledButton(
            onPressed: selectedConn == null || running ? null : _runSql,
            child: running
                ? const SizedBox(
                    width: 14,
                    height: 14,
                    child: CircularProgressIndicator(strokeWidth: 2))
                : const Text('执行'),
          ),
        ]),
      ),
      if (runError.isNotEmpty)
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 8),
          child: Text(runError, style: const TextStyle(color: Colors.redAccent)),
        ),
      Expanded(
        child: ddlText != null
            ? SingleChildScrollView(
                padding: const EdgeInsets.all(8),
                child: SelectableText(ddlText!,
                    style: const TextStyle(fontFamily: 'monospace', fontSize: 12)))
            : result != null
                ? ResultGrid(result: result!)
                : const Center(
                    child: Text('选表看结构，或输入 SQL 执行',
                        style: TextStyle(color: Colors.grey))),
      ),
      if (result != null)
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
          child: Text(
              '${result!.rows.length} 行${result!.truncated ? '（已截断）' : ''}',
              style: const TextStyle(fontSize: 11, color: Colors.grey)),
        ),
    ]);
  }
}
