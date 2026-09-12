// 搜索结果列表（#23 原在 main.dart，#28 拆出）：kind 图标 + db.table。
import 'package:flutter/material.dart';

class SearchList extends StatelessWidget {
  const SearchList({super.key, required this.hits, required this.onPick});

  final List<dynamic>? hits;
  final void Function(String sql) onPick; // 点命中项，语句骨架进编辑器

  @override
  Widget build(BuildContext context) {
    final h = hits;
    if (h == null) {
      return const Center(child: SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2)));
    }
    if (h.isEmpty) {
      return const Center(child: Text('无匹配', style: TextStyle(color: Colors.grey)));
    }
    return ListView.builder(
      itemCount: h.length,
      itemBuilder: (context, i) {
        final m = h[i] as Map<String, dynamic>;
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
            onPick(name.length == 2
                ? 'SELECT *\nFROM ${name[1]};'
                : 'SELECT *\nFROM ${m['table']};');
          },
        );
      },
    );
  }
}
