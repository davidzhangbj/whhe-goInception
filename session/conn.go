// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package session

import (
	"database/sql"
	"fmt"
	"time"
	"strings"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jinzhu/gorm"
	log "github.com/sirupsen/logrus"
)

const maxBadConnRetries = 2

// createNewConnection 用来创建新的连接
// 注意: 该方法可能导致driver: bad connection异常
func (s *session) createNewConnection(dbName string) {
	addr := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=True&loc=Local&autocommit=1&maxAllowedPacket=%d",
		s.opt.User, s.opt.Password, s.opt.Host, s.opt.Port,
		dbName, s.inc.DefaultCharset, s.inc.MaxAllowedPacket)

	db, err := gorm.Open("mysql", addr)

	if err != nil {
		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
		s.appendErrorMsg(err.Error())
		return
	}

	if s.db != nil {
		s.db.Close()
	}

	// 禁用日志记录器，不显示任何日志
	db.LogMode(false)

	// 为保证连接成功关闭,此处等待10ms
	time.Sleep(10 * time.Millisecond)

	s.db = db
}

// raw 执行sql语句,连接失败时自动重连,自动重置当前数据库
func (s *session) raw(sqlStr string) (rows *sql.Rows, err error) {
	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		rows, err = s.db.DB().Query(sqlStr)
		if err == nil {
			return
		}
		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
		if err == mysqlDriver.ErrInvalidConn {
			err1 := s.initConnection()
			if err1 != nil {
				return rows, err1
			}
			s.appendErrorMsg(err.Error())
			continue
		}
		return
	}
	return
}

// exec 执行sql语句,连接失败时自动重连,自动重置当前数据库
func (s *session) exec(sqlStr string, retry bool) (res sql.Result, err error) {
	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		res, err = s.db.DB().Exec(sqlStr)
		if err == nil {
			return
		}
		log.Errorf("con:%d [retry:%v] %v sql:%s",
			s.sessionVars.ConnectionID, i, err, sqlStr)

		if err == mysqlDriver.ErrInvalidConn {
			err1 := s.initConnection()
			if err1 != nil {
				return res, err1
			}
			if retry {
				s.appendWarningMessage(err.Error())
				continue
			}
			return
		}

		// 连接超时时自动重连数据库. 仅在超时设置超过10min时开启该功能
		if s.inc.WaitTimeout >= 600 {
			if myErr, ok := err.(*mysqlDriver.MySQLError); ok &&
				myErr.Number == 1046 && s.dbName != "" {
				err1 := s.initConnection()
				if err1 != nil {
					return res, err1
				}
				s.appendWarningMessage("Database timeout reconnect.")
				continue
			}
		}
		return
	}
	return
}

// execDDL 执行sql语句,连接失败时自动重连,自动重置当前数据库
func (s *session) execDDL(sqlStr string, retry bool) (res sql.Result, err error) {
	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		res, err = s.ddlDB.DB().Exec(sqlStr)
		if err == nil {
			return
		}
		log.Errorf("con:%d %v sql:%s", s.sessionVars.ConnectionID, err, sqlStr)
		if err == mysqlDriver.ErrInvalidConn {
			err1 := s.initConnection()
			if err1 != nil {
				return res, err1
			}
			if retry {
				s.appendWarningMessage(err.Error())
				continue
			}
			return
		}
		return
	}
	return
}

// Raw 执行sql语句,连接失败时自动重连,自动重置当前数据库
func (s *session) rawScan(sqlStr string, dest interface{}) (err error) {
	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		err = s.db.Raw(sqlStr).Scan(dest).Error
		// Scan方法无法直接把EXPLAIN查询的结果映射到dest即OceanBaseQueryPlan，只读取了第一行
		// 所以这里手动把每一行拼一下，赋值给OceanBaseQueryPlan.QueryPlan
		// FORMAT=JSON是OB独有的，所以这里是只处理了OB的EXPLAIN语句
		if strings.Contains(sqlStr,"EXPLAIN FORMAT=JSON") {
			rows, err := s.db.Raw(sqlStr).Rows()
			defer rows.Close()
			if err != nil {
				return err
			}
			var queryPlanSlice []string
			for rows.Next(){
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				// 将每一行添加到queryPalnList切片中
				queryPlanSlice = append(queryPlanSlice, line) 
			}
			// 将切片转换为字符串
			queryPlanString := strings.Join(queryPlanSlice, "")
			// 使用类型断言将 dest 转换为 *OceanBaseQueryPlan
			if queryPlan, ok := dest.(*OceanBaseQueryPlan); ok {
				// 将拼接后的字符串赋值给OceanBaseQueryPlan变量
				queryPlan.QueryPlan = queryPlanString
			}
		}
		if err == nil {
			return
		}
		if err == mysqlDriver.ErrInvalidConn {
			log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
			err1 := s.initConnection()
			if err1 != nil {
				return err1
			}
			s.appendErrorMsg(err.Error())
			continue
		}
		return
	}
	return
}

// Raw 执行sql语句,连接失败时自动重连,自动重置当前数据库
func (s *session) rawDB(dest interface{}, sqlStr string, values ...interface{}) (err error) {
	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		err = s.db.Raw(sqlStr, values...).Scan(dest).Error
		if err == nil {
			return
		}
		if err == mysqlDriver.ErrInvalidConn {
			log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
			err1 := s.initConnection()
			if err1 != nil {
				return err1
			}
			s.appendErrorMsg(err.Error())
			continue
		}
		return
	}
	return
}

// initConnection 连接失败时自动重连,重连后重置当前数据库
func (s *session) initConnection() (err error) {
	name := s.dbName
	if name == "" {
		name = s.opt.db
	}

	// 连接断开无效时,自动重试
	for i := 0; i < maxBadConnRetries; i++ {
		if name == "" {
			err = s.db.DB().Ping()
		} else {
			err = s.db.Exec(fmt.Sprintf("USE `%s`", name)).Error
		}
		if err == nil {
			// 连接重连时,清除线程ID缓存
			// s.threadID = 0
			log.Infof("con:%d Database timeout reconnect", s.sessionVars.ConnectionID)
			return
		}

		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
		if err != mysqlDriver.ErrInvalidConn {
			if myErr, ok := err.(*mysqlDriver.MySQLError); ok {
				s.appendErrorMsg(myErr.Message)
			} else {
				s.appendErrorMsg(err.Error())
			}
			return
		}
	}

	if err != nil {
		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
		if myErr, ok := err.(*mysqlDriver.MySQLError); ok {
			s.appendErrorMsg(myErr.Message)
		} else {
			s.appendErrorMsg(err.Error())
		}
	}
	return
}

// // SwitchDatabase USE切换到当前数据库. (避免连接断开后当前数据库置空)
// func (s *session) SwitchDatabase(db *gorm.DB) error {
// 	name := s.DBName
// 	if name == "" {
// 		name = s.opt.db
// 	}
// 	if name == "" {
// 		return nil
// 	}

// 	// log.Infof("SwitchDatabase: %v", name)
// 	_, err := db.DB().Exec(fmt.Sprintf("USE `%s`", name))
// 	if err != nil {
// 		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
// 		if myErr, ok := err.(*mysqlDriver.MySQLError); ok {
// 			s.AppendErrorMessage(myErr.Message)
// 		} else {
// 			s.AppendErrorMessage(err.Error())
// 		}
// 	}
// 	return err
// }

// // GetDatabase 获取当前数据库
// func (s *session) GetDatabase() string {
// 	log.Debug("GetDatabase")

// 	var value string
// 	sql := "select database();"

// 	rows, err := s.Raw(sql)
// 	if rows != nil {
// 		defer rows.Close()
// 	}

// 	if err != nil {
// 		log.Errorf("con:%d %v", s.sessionVars.ConnectionID, err)
// 		if myErr, ok := err.(*mysqlDriver.MySQLError); ok {
// 			s.AppendErrorMessage(myErr.Message)
// 		} else {
// 			s.AppendErrorMessage(err.Error())
// 		}
// 	} else {
// 		for rows.Next() {
// 			rows.Scan(&value)
// 		}
// 	}

// 	return value
// }
