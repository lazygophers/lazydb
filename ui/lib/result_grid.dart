// 查询结果表格：横向滚动 + 纵向 ListView.builder 虚拟滚动（万行级不卡）。
// ponytail: 固定估宽列，不做每列实测自适应；列内容常被截断时再换 measure-then-layout。
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

class QueryResult {
  final List<String> columns;
  final List<List<dynamic>> rows;
  final bool truncated;
  final int rowsAffected; // 写语句的受影响行数（#24）
  QueryResult(this.columns, this.rows, {this.truncated = false, this.rowsAffected = 0});

  factory QueryResult.fromJson(Map<String, dynamic> j) => QueryResult(
      (j['columns'] as List).cast<String>(),
      (j['rows'] as List).map((r) => (r as List).toList()).toList(),
      truncated: j['truncated'] == true,
      rowsAffected: (j['rows_affected'] as num?)?.toInt() ?? 0);
}

class ResultGrid extends StatelessWidget {
  const ResultGrid({super.key, required this.result});
  final QueryResult result;

  @override
  Widget build(BuildContext context) {
    if (result.columns.isEmpty) {
      return Padding(
          padding: const EdgeInsets.all(12),
          child: Text(result.rowsAffected > 0
              ? '${result.rowsAffected} 行受影响'
              : '执行成功（无结果集）'));
    }
    final widths = _colWidths();
    const cellStyle = TextStyle(fontFamily: 'monospace', fontSize: 12);

    return Scrollbar(
      child: SingleChildScrollView(
        scrollDirection: Axis.horizontal,
        child: SizedBox(
          width: widths.fold<double>(0, (a, b) => a + b),
          child: ListView.builder(
            itemCount: result.rows.length,
            itemExtent: 26,
            itemBuilder: (context, r) {
              final row = result.rows[r];
              final cells = <Widget>[];
              for (var c = 0; c < result.columns.length; c++) {
                final v = c < row.length ? row[c] : null;
                cells.add(SizedBox(
                  width: widths[c],
                  child: Padding(
                    padding: const EdgeInsets.symmetric(horizontal: 6),
                    child: Text(v == null ? 'NULL' : '$v',
                        maxLines: 1,
                        softWrap: false,
                        overflow: TextOverflow.ellipsis,
                        style: cellStyle),
                  ),
                ));
              }
              return Material(
                color: r.isEven ? const Color(0xFF1E2128) : const Color(0xFF23262E),
                child: InkWell(
                  onLongPress: () =>
                      Clipboard.setData(ClipboardData(text: row.join('\t'))),
                  child: Row(
                      crossAxisAlignment: CrossAxisAlignment.center,
                      children: cells),
                ),
              );
            },
          ),
        ),
      ),
    );
  }

  // 表头 + 前 50 行估宽，夹在 64..280。
  List<double> _colWidths() {
    return List.generate(result.columns.length, (c) {
      var w = (result.columns[c].length + 2) * 7.0;
      for (var r = 0; r < result.rows.length && r < 50; r++) {
        final v = c < result.rows[r].length ? result.rows[r][c] : null;
        w = math.max(w, (v == null ? 4 : '$v'.length) * 7.0);
      }
      return w.clamp(64.0, 280.0) + 12;
    });
  }
}
