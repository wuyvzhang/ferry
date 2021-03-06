package service

import (
	"encoding/json"
	"errors"
	"ferry/global/orm"
	"ferry/models/base"
	"ferry/models/process"
	"ferry/models/system"
	"ferry/tools"
	"ferry/tools/app"
	"fmt"
	"reflect"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/gin-gonic/gin"
)

/*
  @Author : lanyulei
  @Desc : 处理工单
*/

/*
    -- 节点 --
	start: 开始节点
	userTask: 审批节点
	receiveTask: 处理节点
	scriptTask: 任务节点
	end: 结束节点

    -- 网关 --
    exclusiveGateway: 排他网关
    parallelGateway: 并行网关
    inclusiveGateway: 包容网关

*/

type Handle struct {
	cirHistoryList   []process.CirculationHistory
	workOrderId      int
	updateValue      map[string]interface{}
	stateValue       map[string]interface{}
	targetStateValue map[string]interface{}
	workOrderData    [][]byte
	workOrderDetails process.WorkOrderInfo
	endHistory       bool
	flowProperties   int
	circulationValue string
	processState     ProcessState
	tx               *gorm.DB
}

// 时间格式化
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	return fmt.Sprintf("%02d小时 %02d分钟", h, m)
}

// 会签
func (h *Handle) Countersign(c *gin.Context) (err error) {
	var (
		stateList       []map[string]interface{}
		stateIdMap      map[string]interface{}
		currentState    map[string]interface{}
		cirHistoryCount int
	)

	err = json.Unmarshal(h.workOrderDetails.State, &stateList)
	if err != nil {
		return
	}

	stateIdMap = make(map[string]interface{})
	for _, v := range stateList {
		stateIdMap[v["id"].(string)] = v["label"]
		if v["id"].(string) == h.stateValue["id"].(string) {
			currentState = v
		}
	}
	for _, cirHistoryValue := range h.cirHistoryList {
		if _, ok := stateIdMap[cirHistoryValue.Source]; !ok {
			break
		}
		for _, processor := range currentState["processor"].([]interface{}) {
			if cirHistoryValue.ProcessorId != tools.GetUserId(c) &&
				cirHistoryValue.Source == currentState["id"].(string) &&
				cirHistoryValue.ProcessorId == int(processor.(float64)) {
				cirHistoryCount += 1
			}
		}
	}
	if cirHistoryCount == len(currentState["processor"].([]interface{}))-1 {
		h.endHistory = true
		err = h.circulation()
		if err != nil {
			return
		}
	}
	return
}

// 工单跳转
func (h *Handle) circulation() (err error) {
	var (
		stateValue []byte
	)

	err = GetVariableValue(h.updateValue["state"].([]interface{}), h.workOrderDetails.Creator)
	if err != nil {
		return
	}

	stateValue, err = json.Marshal(h.updateValue["state"])
	if err != nil {
		return
	}

	err = h.tx.Model(&process.WorkOrderInfo{}).
		Where("id = ?", h.workOrderId).
		Updates(map[string]interface{}{
			"state":          stateValue,
			"related_person": h.updateValue["related_person"],
		}).Error
	if err != nil {
		h.tx.Rollback()
		return
	}
	return
}

// 条件判断
func (h *Handle) ConditionalJudgment(condExpr map[string]interface{}) (result bool, err error) {
	var (
		condExprOk    bool
		condExprValue interface{}
	)

	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case string:
				err = errors.New(e)
			case error:
				err = e
			default:
				err = errors.New("未知错误")
			}
			return
		}
	}()

	for _, data := range h.workOrderData {
		var formData map[string]interface{}
		err = json.Unmarshal(data, &formData)
		if err != nil {
			return
		}
		if condExprValue, condExprOk = formData[condExpr["key"].(string)]; condExprOk {
			break
		}
	}

	if condExprValue == nil {
		err = errors.New("未查询到对应的表单数据。")
		return
	}

	// todo 待优化
	switch reflect.TypeOf(condExprValue).String() {
	case "string":
		switch condExpr["sign"] {
		case "==":
			if condExprValue.(string) == condExpr["value"].(string) {
				result = true
			}
		case "!=":
			if condExprValue.(string) != condExpr["value"].(string) {
				result = true
			}
		case ">":
			if condExprValue.(string) > condExpr["value"].(string) {
				result = true
			}
		case ">=":
			if condExprValue.(string) >= condExpr["value"].(string) {
				result = true
			}
		case "<":
			if condExprValue.(string) < condExpr["value"].(string) {
				result = true
			}
		case "<=":
			if condExprValue.(string) <= condExpr["value"].(string) {
				result = true
			}
		default:
			err = errors.New("目前仅支持6种常规判断类型，包括（等于、不等于、大于、大于等于、小于、小于等于）")
		}
	case "float64":
		switch condExpr["sign"] {
		case "==":
			if condExprValue.(float64) == condExpr["value"].(float64) {
				result = true
			}
		case "!=":
			if condExprValue.(float64) != condExpr["value"].(float64) {
				result = true
			}
		case ">":
			if condExprValue.(float64) > condExpr["value"].(float64) {
				result = true
			}
		case ">=":
			if condExprValue.(float64) >= condExpr["value"].(float64) {
				result = true
			}
		case "<":
			if condExprValue.(float64) < condExpr["value"].(float64) {
				result = true
			}
		case "<=":
			if condExprValue.(float64) <= condExpr["value"].(float64) {
				result = true
			}
		default:
			err = errors.New("目前仅支持6种常规判断类型，包括（等于、不等于、大于、大于等于、小于、小于等于）")
		}
	default:
		err = errors.New("条件判断目前仅支持字符串、整型。")
	}

	return
}

// 并行网关，确认其他节点是否完成
func (h *Handle) completeAllParallel(c *gin.Context, target string) (statusOk bool, err error) {
	var (
		stateList []map[string]interface{}
	)

	err = json.Unmarshal(h.workOrderDetails.State, &stateList)
	if err != nil {
		err = fmt.Errorf("反序列化失败，%v", err.Error())
		return
	}

continueHistoryTag:
	for _, v := range h.cirHistoryList {
		status := false
		for i, s := range stateList {
			if v.Source == s["id"].(string) && v.Target == target {
				status = true
				stateList = append(stateList[:i], stateList[i+1:]...)
				continue continueHistoryTag
			}
		}
		if !status {
			break
		}
	}

	if len(stateList) == 1 && stateList[0]["id"].(string) == h.stateValue["id"] {
		statusOk = true
	}

	return
}

func (h *Handle) commonProcessing(c *gin.Context) (err error) {
	// 如果是拒绝的流转则直接跳转
	if h.flowProperties == 0 {
		err = h.circulation()
		if err != nil {
			err = fmt.Errorf("工单跳转失败，%v", err.Error())
		}
		return
	}

	// 会签
	if h.stateValue["assignValue"] != nil && len(h.stateValue["assignValue"].([]interface{})) > 1 {
		if isCounterSign, ok := h.stateValue["isCounterSign"]; ok {
			if isCounterSign.(bool) {
				h.endHistory = false
				err = h.Countersign(c)
				if err != nil {
					return
				}
			} else {
				err = h.circulation()
				if err != nil {
					return
				}
			}
		} else {
			err = h.circulation()
			if err != nil {
				return
			}
		}
	} else {
		err = h.circulation()
		if err != nil {
			return
		}
	}
	return
}

func (h *Handle) HandleWorkOrder(
	c *gin.Context,
	workOrderId int,
	tasks []string,
	targetState string,
	sourceState string,
	circulationValue string,
	flowProperties int,
) (err error) {
	h.workOrderId = workOrderId
	h.flowProperties = flowProperties
	h.endHistory = true

	var (
		execTasks          []string
		relatedPersonList  []int
		cirHistoryValue    []process.CirculationHistory
		cirHistoryData     process.CirculationHistory
		costDurationValue  string
		sourceEdges        []map[string]interface{}
		targetEdges        []map[string]interface{}
		condExprStatus     bool
		relatedPersonValue []byte
		parallelStatusOk   bool
		processInfo        process.Info
		currentUserInfo    system.SysUser
	)

	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case string:
				err = errors.New(e)
			case error:
				err = e
			default:
				err = errors.New("未知错误")
			}
			return
		}
	}()

	// 获取工单信息
	err = orm.Eloquent.Model(&process.WorkOrderInfo{}).Where("id = ?", workOrderId).Find(&h.workOrderDetails).Error
	if err != nil {
		return
	}

	// 获取流程信息
	err = orm.Eloquent.Model(&process.Info{}).Where("id = ?", h.workOrderDetails.Process).Find(&processInfo).Error
	if err != nil {
		return
	}
	err = json.Unmarshal(processInfo.Structure, &h.processState.Structure)
	if err != nil {
		return
	}

	// 获取当前节点
	h.stateValue, err = h.processState.GetNode(sourceState)
	if err != nil {
		return
	}

	// 目标状态
	h.targetStateValue, err = h.processState.GetNode(targetState)
	if err != nil {
		return
	}

	// 获取工单数据
	err = orm.Eloquent.Model(&process.TplData{}).
		Where("work_order = ?", workOrderId).
		Pluck("form_data", &h.workOrderData).Error
	if err != nil {
		return
	}

	// 根据处理人查询出需要会签的条数
	err = orm.Eloquent.Model(&process.CirculationHistory{}).
		Where("work_order = ?", workOrderId).
		Order("id desc").
		Find(&h.cirHistoryList).Error
	if err != nil {
		return
	}

	err = json.Unmarshal(h.workOrderDetails.RelatedPerson, &relatedPersonList)
	if err != nil {
		return
	}
	relatedPersonStatus := false
	for _, r := range relatedPersonList {
		if r == tools.GetUserId(c) {
			relatedPersonStatus = true
			break
		}
	}
	if !relatedPersonStatus {
		relatedPersonList = append(relatedPersonList, tools.GetUserId(c))
	}

	relatedPersonValue, err = json.Marshal(relatedPersonList)
	if err != nil {
		return
	}

	h.updateValue = map[string]interface{}{
		"related_person": relatedPersonValue,
	}

	// 开启事务
	h.tx = orm.Eloquent.Begin()

	stateValue := map[string]interface{}{
		"label": h.targetStateValue["label"].(string),
		"id":    h.targetStateValue["id"].(string),
	}

	switch h.targetStateValue["clazz"] {
	// 排他网关
	case "exclusiveGateway":
		sourceEdges, err = h.processState.GetEdge(h.targetStateValue["id"].(string), "source")
		if err != nil {
			return
		}
	breakTag:
		for _, edge := range sourceEdges {
			edgeCondExpr := make([]map[string]interface{}, 0)
			err = json.Unmarshal([]byte(edge["conditionExpression"].(string)), &edgeCondExpr)
			if err != nil {
				return
			}
			for _, condExpr := range edgeCondExpr {
				// 条件判断
				condExprStatus, err = h.ConditionalJudgment(condExpr)
				if err != nil {
					return
				}
				if condExprStatus {
					// 进行节点跳转
					h.targetStateValue, err = h.processState.GetNode(edge["target"].(string))
					if err != nil {
						return
					}

					if h.targetStateValue["clazz"] == "userTask" || h.targetStateValue["clazz"] == "receiveTask" {
						if h.targetStateValue["assignValue"] == nil || h.targetStateValue["assignType"] == "" {
							err = errors.New("处理人不能为空")
							return
						}
					}

					h.updateValue["state"] = []map[string]interface{}{{
						"id":             h.targetStateValue["id"].(string),
						"label":          h.targetStateValue["label"],
						"processor":      h.targetStateValue["assignValue"],
						"process_method": h.targetStateValue["assignType"],
					}}
					err = h.commonProcessing(c)
					if err != nil {
						err = fmt.Errorf("流程流程跳转失败，%v", err.Error())
						return
					}

					break breakTag
				}
			}
		}
		if !condExprStatus {
			err = errors.New("所有流转均不符合条件，请确认。")
			return
		}
	// 并行/聚合网关
	case "parallelGateway":
		// 入口，判断
		sourceEdges, err = h.processState.GetEdge(h.targetStateValue["id"].(string), "source")
		if err != nil {
			err = fmt.Errorf("查询流转信息失败，%v", err.Error())
			return
		}

		targetEdges, err = h.processState.GetEdge(h.targetStateValue["id"].(string), "target")
		if err != nil {
			err = fmt.Errorf("查询流转信息失败，%v", err.Error())
			return
		}

		if len(sourceEdges) > 0 {
			h.targetStateValue, err = h.processState.GetNode(sourceEdges[0]["target"].(string))
			if err != nil {
				return
			}
		} else {
			err = errors.New("并行网关流程不正确")
			return
		}

		if len(sourceEdges) > 1 && len(targetEdges) == 1 {
			// 入口
			h.updateValue["state"] = make([]map[string]interface{}, 0)
			for _, edge := range sourceEdges {
				targetStateValue, err := h.processState.GetNode(edge["target"].(string))
				if err != nil {
					return err
				}
				h.updateValue["state"] = append(h.updateValue["state"].([]map[string]interface{}), map[string]interface{}{
					"id":             edge["target"].(string),
					"label":          targetStateValue["label"],
					"processor":      targetStateValue["assignValue"],
					"process_method": targetStateValue["assignType"],
				})
			}
			err = h.circulation()
			if err != nil {
				err = fmt.Errorf("工单跳转失败，%v", err.Error())
				return
			}
		} else if len(sourceEdges) == 1 && len(targetEdges) > 1 {
			// 出口
			parallelStatusOk, err = h.completeAllParallel(c, sourceEdges[0]["target"].(string))
			if err != nil {
				err = fmt.Errorf("并行检测失败，%v", err.Error())
				return
			}
			if parallelStatusOk {
				h.endHistory = true
				h.updateValue["state"] = []map[string]interface{}{{
					"id":             h.targetStateValue["id"].(string),
					"label":          h.targetStateValue["label"],
					"processor":      h.targetStateValue["assignValue"],
					"process_method": h.targetStateValue["assignType"],
				}}
				err = h.circulation()
				if err != nil {
					err = fmt.Errorf("工单跳转失败，%v", err.Error())
					return
				}
			} else {
				h.endHistory = false
			}

		} else {
			err = errors.New("并行网关流程不正确")
			return
		}
	// 包容网关
	case "inclusiveGateway":
		fmt.Println("inclusiveGateway")
		return
	case "start":
		stateValue["processor"] = []int{h.workOrderDetails.Creator}
		stateValue["process_method"] = "person"
		h.updateValue["state"] = []interface{}{stateValue}
		err = h.circulation()
		if err != nil {
			return
		}
	case "userTask":
		stateValue["processor"] = h.targetStateValue["assignValue"].([]interface{})
		stateValue["process_method"] = h.targetStateValue["assignType"].(string)
		h.updateValue["state"] = []interface{}{stateValue}
		err = h.commonProcessing(c)
		if err != nil {
			return
		}
	case "receiveTask":
		stateValue["processor"] = h.targetStateValue["assignValue"].([]interface{})
		stateValue["process_method"] = h.targetStateValue["assignType"].(string)
		h.updateValue["state"] = []interface{}{stateValue}
		err = h.commonProcessing(c)
		if err != nil {
			return
		}
	case "scriptTask":
		stateValue["processor"] = []int{}
		stateValue["process_method"] = ""
		h.updateValue["state"] = []interface{}{stateValue}
	case "end":
		stateValue["processor"] = []int{}
		stateValue["process_method"] = ""
		h.updateValue["state"] = []interface{}{stateValue}
		err = h.circulation()
		if err != nil {
			h.tx.Rollback()
			return
		}
		err = h.tx.Model(&process.WorkOrderInfo{}).
			Where("id = ?", h.workOrderId).
			Update("is_end", 1).Error
		if err != nil {
			h.tx.Rollback()
			return
		}
	}

	// 流转历史写入
	err = orm.Eloquent.Model(&cirHistoryValue).
		Where("work_order = ?", workOrderId).
		Find(&cirHistoryValue).
		Order("create_time desc").Error
	if err != nil {
		h.tx.Rollback()
		return
	}
	for _, t := range cirHistoryValue {
		if t.Source != h.stateValue["id"] {
			costDuration := time.Since(t.CreatedAt.Time)
			costDurationValue = fmtDuration(costDuration)
		}
	}

	// 获取当前用户信息
	err = orm.Eloquent.Model(&currentUserInfo).
		Where("user_id = ?", tools.GetUserId(c)).
		Find(&currentUserInfo).Error
	if err != nil {
		app.Error(c, -1, err, fmt.Sprintf("当前用户查询失败，%v", err.Error()))
		return
	}

	cirHistoryData = process.CirculationHistory{
		Model:        base.Model{},
		Title:        h.workOrderDetails.Title,
		WorkOrder:    h.workOrderDetails.Id,
		State:        h.stateValue["label"].(string),
		Source:       h.stateValue["id"].(string),
		Target:       h.targetStateValue["id"].(string),
		Circulation:  circulationValue,
		Processor:    currentUserInfo.NickName,
		ProcessorId:  tools.GetUserId(c),
		CostDuration: costDurationValue,
	}

	err = h.tx.Create(&cirHistoryData).Error
	if err != nil {
		h.tx.Rollback()
		return
	}

	// 判断目标是否是结束节点
	if h.targetStateValue["clazz"] == "end" && h.endHistory == true {
		err = h.tx.Create(&process.CirculationHistory{
			Model:       base.Model{},
			Title:       h.workOrderDetails.Title,
			WorkOrder:   h.workOrderDetails.Id,
			State:       h.targetStateValue["label"].(string),
			Source:      h.targetStateValue["id"].(string),
			Processor:   currentUserInfo.NickName,
			ProcessorId: tools.GetUserId(c),
			Circulation: "结束",
		}).Error
		if err != nil {
			h.tx.Rollback()
			return
		}
	}

	h.tx.Commit() // 提交事务

	// 执行流程公共任务及节点任务
	if h.stateValue["task"] != nil {
		for _, task := range h.stateValue["task"].([]interface{}) {
			tasks = append(tasks, task.(string))
		}
	}
continueTag:
	for _, task := range tasks {
		for _, t := range execTasks {
			if t == task {
				continue continueTag
			}
		}
		execTasks = append(execTasks, task)
	}
	go ExecTask(execTasks)

	return
}
