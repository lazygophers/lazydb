// 界面层唯一的基础设施：附着/拉起 Go sidecar，包一层 HTTP 客户端（ADR-0001：
// 界面薄，不写业务逻辑，一切经 API）。
import 'dart:async';
import 'dart:convert';
import 'dart:io';

class ApiError implements Exception {
  final int status;
  final String code;
  final String message;
  ApiError(this.status, this.code, this.message);
  @override
  String toString() => '$code: $message';
}

class Backend {
  Backend(this.base, this.token);
  final String base;
  final String token;
  final _http = HttpClient()..connectionTimeout = const Duration(seconds: 10);

  static String userHome() => Platform.isWindows
      ? (Platform.environment['USERPROFILE'] ?? '')
      : (Platform.environment['HOME'] ?? '');

  static File runtimeFile() => File('${userHome()}/.lazydb/runtime');

  /// 附着已在跑的后端（读 ~/.lazydb/runtime 后探活）；
  /// 不在或探活失败则拉起 [bin] 再附着。
  static Future<Backend> attach(String bin) async {
    final existing = await _tryAttach();
    if (existing != null) return existing;

    try {
      await Process.start(bin, const [], mode: ProcessStartMode.detached);
    } on ProcessException catch (e) {
      throw Exception('后端拉起失败（$bin）：${e.message}');
    }
    // sidecar 自己写 runtime 文件，轮询等它出现再探活。
    for (var i = 0; i < 50; i++) {
      await Future<void>.delayed(const Duration(milliseconds: 200));
      final b = await _tryAttach();
      if (b != null) return b;
    }
    throw Exception('后端拉起后 10s 内未就绪');
  }

  static Future<Backend?> _tryAttach() async {
    try {
      final info = jsonDecode(await runtimeFile().readAsString());
      final b = Backend(
          'http://127.0.0.1:${info['port']}', info['token'] as String);
      await b.health();
      return b;
    } catch (_) {
      return null; // 文件不在/损坏/探活失败都视为不在
    }
  }

  /// 找 sidecar 二进制：LAZYDB_BIN 优先，其次 cwd 与可执行文件同目录。
  static String findBinary() {
    final env = Platform.environment['LAZYDB_BIN'];
    if (env != null && env.isNotEmpty) return env;
    if (File('lazydb').existsSync()) return 'lazydb';
    final beside = File('${File(Platform.resolvedExecutable).parent.path}/lazydb');
    if (beside.existsSync()) return beside.path;
    return 'lazydb'; // 交给 PATH
  }

  // ---- HTTP ----

  Future<dynamic> _json(String method, String path, {Object? body}) async {
    final req = await _http.openUrl(method, Uri.parse('$base$path'));
    req.headers.set('Authorization', 'Bearer $token');
    if (body != null) {
      final b = utf8.encode(jsonEncode(body));
      req.headers.contentLength = b.length;
      req.headers.contentType = ContentType.json;
      req.add(b);
    }
    final res = await req.close();
    final text = await res.transform(utf8.decoder).join();
    final data = text.isEmpty ? null : jsonDecode(text);
    if (res.statusCode >= 400) {
      final e = (data as Map?)?['error'];
      throw ApiError(res.statusCode, e?['code'] ?? 'error',
          e?['message'] ?? res.reasonPhrase ?? '');
    }
    return data;
  }

  Future<void> health() async => _json('GET', '/api/health');

  Future<List<dynamic>> listConnections() async =>
      (await _json('GET', '/api/connections')) as List<dynamic>;

  Future<dynamic> createConnection(String name, Map<String, dynamic> cfg) =>
      _json('POST', '/api/connections', body: {'name': name, 'config': cfg});

  Future<void> deleteConnection(String id) => _json('DELETE', '/api/connections/$id');

  Future<void> ping(String id) => _json('POST', '/api/connections/$id/ping');

  Future<List<dynamic>> children(String id, List<String> path) async {
    final q = path.map((p) => 'path=${Uri.encodeQueryComponent(p)}').join('&');
    final r = await _json('GET', '/api/connections/$id/children${q.isEmpty ? '' : '?$q'}');
    return r['nodes'] as List<dynamic>;
  }

  Future<dynamic> exec(String id, String sql, {int maxRows = 10000}) =>
      _json('POST', '/api/connections/$id/exec', body: {'sql': sql, 'max_rows': maxRows});

  Future<dynamic> columns(String id, List<String> path) async =>
      _json('GET', '/api/connections/$id/columns${_q(path)}');

  Future<dynamic> indexes(String id, List<String> path) async =>
      _json('GET', '/api/connections/$id/indexes${_q(path)}');

  Future<String> ddl(String id, List<String> path) async =>
      (await _json('GET', '/api/connections/$id/ddl${_q(path)}'))['ddl'] as String;

  Future<dynamic> capabilities(String id) =>
      _json('GET', '/api/connections/$id/capabilities');

  Future<void> refresh(String id, List<String> path) =>
      _json('POST', '/api/connections/$id/refresh', body: {'path': path});

  Future<dynamic> testConnection(Map<String, dynamic> cfg) =>
      _json('POST', '/api/test-connection', body: {'config': cfg});

  Future<List<dynamic>> search(String id, String q) async {
    final r = await _json('GET',
        '/api/connections/$id/search?q=${Uri.encodeQueryComponent(q)}');
    return r['matches'] as List<dynamic>;
  }

  Future<List<dynamic>> history(String q) async {
    final r = await _json(
        'GET', '/api/history?q=${Uri.encodeQueryComponent(q)}');
    return r['items'] as List<dynamic>;
  }

  static String _q(List<String> path) =>
      path.map((p) => 'path=${Uri.encodeQueryComponent(p)}').join('&');
}
