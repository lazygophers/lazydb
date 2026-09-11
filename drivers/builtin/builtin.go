// Package builtin 是内置驱动的注册表（#18：tracer bullet 不依赖下载）。
// #22 插件化时这里换成驱动代理拉起。
package builtin

import (
	"fmt"

	"github.com/lazygophers/lazydb/internal/mysqlsrc"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

// Open 按驱动名构造 Source。
func Open(cfg source.Config) (source.Source, error) {
	switch cfg.Driver {
	case "sqlite":
		return &sqlsrc.SQLite{}, nil
	case "mysql":
		return &mysqlsrc.MySQL{}, nil
	}
	return nil, fmt.Errorf("unknown driver %q", cfg.Driver)
}
