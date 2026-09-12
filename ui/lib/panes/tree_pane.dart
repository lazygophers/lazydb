// 结构树面板（#28 从 main.dart 拆出）：懒加载树 + 搜索框 + 搜索结果切换。
import 'package:flutter/material.dart';

import '../backend.dart';
import 'search_list.dart';

// 树节点（纯 UI 状态，展开即经 API 懒加载子层）
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

class TreePane extends StatefulWidget {
  const TreePane({
    super.key,
    required this.be,
    required this.connId,
    required this.connName,
    required this.trees, // connID → 根层节点（父层持有，刷新即清）
    required this.onColumns, // 看过的表列进补全词
    required this.onDdl, // 点 DDL 节点，DDL 展示归结果区
    required this.onPickSql, // 点搜索命中，把语句骨架带进编辑器
  });

  final Backend be;
  final String connId;
  final String connName;
  final Map<String, List<TreeNode>> trees;
  final void Function(String table, List<Map<String, dynamic>> cols) onColumns;
  final Future<void> Function(String connId, TreeNode n) onDdl;
  final void Function(String sql) onPickSql;

  @override
  State<TreePane> createState() => _TreePaneState();
}

class _TreePaneState extends State<TreePane> {
  // 结构搜索（#23）：非空时结构树换成搜索结果。
  final searchCtl = TextEditingController();
  List<dynamic>? searchHits;

  @override
  void dispose() {
    searchCtl.dispose();
    super.dispose();
  }

  Future<void> _toggle(String connId, TreeNode n) async {
    if (n.kind == 'ddl') {
      await widget.onDdl(connId, n);
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
          for (final node in await widget.be.children(connId, [])) {
            kids.add(TreeNode(node['name'], node['kind'], [node['name']]));
          }
        case 'database':
          for (final node in await widget.be.children(connId, n.path)) {
            final t = TreeNode(node['name'], node['kind'], [...n.path, node['name']]);
            kids.add(t);
          }
        case 'table':
        case 'view':
          kids.addAll([
            TreeNode('字段', 'cols', n.path),
            TreeNode('索引', 'idx', n.path),
            TreeNode('外键', 'fk', n.path),
            TreeNode('DDL', 'ddl', n.path),
          ]);
        case 'cols':
          final r = await widget.be.columns(connId, n.path);
          final cols = (r['columns'] as List).cast<Map<String, dynamic>>();
          widget.onColumns(n.path.last, cols); // 树里看过的表直接进补全词
          for (final c in cols) {
            final nullTxt = c['nullable'] == true ? '' : ' NOT NULL';
            kids.add(TreeNode(
                '${c['name']}  ${c['type']}$nullTxt', 'leaf', n.path));
          }
        case 'idx':
          final r = await widget.be.indexes(connId, n.path);
          for (final i in r['indexes']) {
            final u = i['unique'] == true ? 'UNIQUE ' : '';
            kids.add(TreeNode(
                '$u${i['name']} (${(i['columns'] as List).join(', ')})',
                'leaf',
                n.path));
          }
        case 'fk': // 外键页签（#34）：列 → 引用表(列)，含 ON DELETE/UPDATE 动作
          final r = await widget.be.foreignKeys(connId, n.path);
          for (final f in r['foreign_keys']) {
            kids.add(TreeNode(
                '${(f['columns'] as List).join(', ')} → ${f['ref_table']}(${(f['ref_columns'] as List).join(', ')})  ON DELETE ${f['on_delete']} / ON UPDATE ${f['on_update']}',
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

  Future<void> _search(String q) async {
    if (q.isEmpty) {
      setState(() => searchHits = null);
      return;
    }
    try {
      final hits = await widget.be.search(widget.connId, q);
      if (searchCtl.text == q) setState(() => searchHits = hits);
    } catch (_) {
      setState(() => searchHits = const []);
    }
  }

  @override
  Widget build(BuildContext context) {
    final roots = widget.trees.putIfAbsent(widget.connId, () => []);
    if (roots.isEmpty) {
      // 根 = 连接自身
      roots.add(TreeNode(widget.connName, 'conn', []));
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
              await widget.be.refresh(widget.connId, []);
              widget.trees.remove(widget.connId);
              setState(() {});
            }),
        ]),
      ),
      Expanded(
        child: searchCtl.text.isNotEmpty
            ? SearchList(
                hits: searchHits,
                onPick: widget.onPickSql,
              )
            : ListView(
                children: [for (final n in roots) _treeNode(widget.connId, n, 0)]),
      ),
    ]);
  }

  Widget _treeNode(String connId, TreeNode n, int depth) {
    final expandable = switch (n.kind) {
      'conn' || 'database' || 'table' || 'view' || 'cols' || 'idx' || 'fk' => true,
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
}
