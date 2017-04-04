package model

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"common"
	"logger"
	"pipelines"
)

//
// Pipeline context
//

type bucketItem struct {
	bucket     pipelines.PipelineContextDataBucket
	autoDelete bool
}

type pipelineContext struct {
	pipeName         string
	plugNames        []string
	parallelismCount uint16
	statistics       pipelines.PipelineStatistics
	mod              *Model
	bucketLocker     sync.RWMutex // dedicated lock provides better performance
	buckets          map[string]*bucketItem
	bookLocker       sync.RWMutex
	preparationBook  map[string]interface{} // hash here for O(1) time on query
}

func NewPipelineContext(conf pipelines.Config,
	statistics pipelines.PipelineStatistics, m *Model) *pipelineContext {

	return &pipelineContext{
		pipeName:         conf.PipelineName(),
		plugNames:        conf.PluginNames(),
		parallelismCount: conf.Parallelism(),
		statistics:       statistics,
		mod:              m,
		buckets:          make(map[string]*bucketItem),
		preparationBook:  make(map[string]interface{}),
	}
}

func (pc *pipelineContext) PipelineName() string {
	return pc.pipeName
}

func (pc *pipelineContext) PluginNames() []string {
	return pc.plugNames
}

func (pc *pipelineContext) Parallelism() uint16 {
	return pc.parallelismCount
}

func (pc *pipelineContext) Statistics() pipelines.PipelineStatistics {
	return pc.statistics
}

func (pc *pipelineContext) DataBucket(pluginName, pluginInstanceId string) pipelines.PipelineContextDataBucket {
	deleteWhenPluginUpdatedOrDeleted := false

	if len(strings.TrimSpace(pluginInstanceId)) == 0 {
		pluginInstanceId = pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE
	} else if pluginInstanceId != pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE {
		deleteWhenPluginUpdatedOrDeleted = true
	}

	pc.bucketLocker.Lock()

	bucketKey := fmt.Sprintf("%s-%s", pluginName, pluginInstanceId)

	item, exists := pc.buckets[bucketKey]
	if exists {
		defer pc.bucketLocker.Unlock()
		return item.bucket
	}

	bucket := newPipelineContextDataBucket()

	pc.buckets[bucketKey] = &bucketItem{
		bucket:     bucket,
		autoDelete: deleteWhenPluginUpdatedOrDeleted,
	}

	defer pc.bucketLocker.Unlock()

	if deleteWhenPluginUpdatedOrDeleted {
		pc.mod.AddPluginDeletedCallback("deletePipelineContextDataBucketWhenPluginDeleted",
			pc.deletePipelineContextDataBucketWhenPluginDeleted, false)
		pc.mod.AddPluginUpdatedCallback("deletePipelineContextDataBucketWhenPluginUpdated",
			pc.deletePipelineContextDataBucketWhenPluginUpdated, false)
	}

	return bucket
}

func (pc *pipelineContext) DeleteBucket(pluginName, pluginInstanceId string) pipelines.PipelineContextDataBucket {
	if len(strings.TrimSpace(pluginInstanceId)) == 0 {
		pluginInstanceId = pipelines.DATA_BUCKET_FOR_ALL_PLUGIN_INSTANCE
	}

	var oriBucket pipelines.PipelineContextDataBucket
	updatedBucket := make(map[string]*bucketItem)

	pc.bucketLocker.Lock()
	defer pc.bucketLocker.Unlock()

	for bucketKey, bucketItem := range pc.buckets {
		if bucketKey == fmt.Sprintf("%s-%s", pluginName, pluginInstanceId) {
			oriBucket = bucketItem.bucket
		} else {
			updatedBucket[bucketKey] = bucketItem
		}
	}

	pc.buckets = updatedBucket

	return oriBucket
}

func (pc *pipelineContext) PreparePlugin(pluginName string, fun pipelines.PluginPreparationFunc) {
	pc.bookLocker.RLock()
	_, exists := pc.preparationBook[pluginName]
	if exists {
		pc.bookLocker.RUnlock()
		return
	}
	pc.bookLocker.RUnlock()

	pc.bookLocker.Lock()
	defer pc.bookLocker.Unlock()

	// DCL
	_, exists = pc.preparationBook[pluginName]
	if exists {
		return
	}

	logger.Debugf("[prepare plugin %s for pipeline %s]", pluginName, pc.pipeName)
	fun()
	logger.Debugf("[plugin %s prepared for pipeline %s]", pluginName, pc.pipeName)

	pc.mod.AddPluginDeletedCallback("deletePluginPreparationBookWhenPluginUpdatedOrDeleted",
		pc.deletePluginPreparationBookWhenPluginUpdatedOrDeleted, false)
	pc.mod.AddPluginUpdatedCallback("deletePluginPreparationBookWhenPluginUpdatedOrDeleted",
		pc.deletePluginPreparationBookWhenPluginUpdatedOrDeleted, false)

	pc.preparationBook[pluginName] = nil
}

func (pc *pipelineContext) Close() {
	pc.mod.DeletePluginDeletedCallback("deletePipelineContextDataBucketWhenPluginDeleted")
	pc.mod.DeletePluginUpdatedCallback("deletePipelineContextDataBucketWhenPluginUpdated")

	pc.mod.DeletePluginDeletedCallback("deletePluginPreparationBookWhenPluginUpdatedOrDeleted")
	pc.mod.DeletePluginUpdatedCallback("deletePluginPreparationBookWhenPluginUpdatedOrDeleted")
}

func (pc *pipelineContext) deletePluginPreparationBookWhenPluginUpdatedOrDeleted(plugin *Plugin) {
	if !common.StrInSlice(plugin.Name(), pc.plugNames) {
		return
	}

	pc.bookLocker.Lock()
	defer pc.bookLocker.Unlock()
	delete(pc.preparationBook, plugin.Name())
}

func (pc *pipelineContext) deletePipelineContextDataBucketWhenPluginDeleted(_ *Plugin) {
	bucketInUsed := func(bucketKey string) bool {
		// defensive the case plugin instance closes after it was deleted from model
		for _, pluginName := range pc.PluginNames() {
			if strings.HasPrefix(bucketKey, fmt.Sprintf("%s-", pluginName)) {
				return true
			}
		}

		return false
	}

	pc.bucketLocker.Lock()
	defer pc.bucketLocker.Unlock()

	updatedBucket := make(map[string]*bucketItem)

	for bucketKey, bucketItem := range pc.buckets {
		if bucketInUsed(bucketKey) || !bucketItem.autoDelete {
			updatedBucket[bucketKey] = bucketItem
		}
	}

	pc.buckets = updatedBucket
}

func (pc *pipelineContext) deletePipelineContextDataBucketWhenPluginUpdated(p *Plugin) {
	updatedBucket := make(map[string]*bucketItem)

	pc.bucketLocker.Lock()
	defer pc.bucketLocker.Unlock()

	for bucketKey, bucketItem := range pc.buckets {
		if !strings.HasPrefix(bucketKey, fmt.Sprintf("%s-", p.Name())) || !bucketItem.autoDelete {
			updatedBucket[bucketKey] = bucketItem
		}
	}

	pc.buckets = updatedBucket
}

//
// Pipeline context data bucket
//

type pipelineContextDataBucket struct {
	sync.RWMutex
	data map[interface{}]interface{}
}

func newPipelineContextDataBucket() *pipelineContextDataBucket {
	return &pipelineContextDataBucket{
		data: make(map[interface{}]interface{}),
	}
}

func (b *pipelineContextDataBucket) BindData(key, value interface{}) (interface{}, error) {
	b.Lock()
	defer b.Unlock()
	return b.bindData(key, value)
}

func (b *pipelineContextDataBucket) bindData(key, value interface{}) (interface{}, error) {
	if key == nil {
		return nil, fmt.Errorf("key is nil")
	}

	if !reflect.TypeOf(key).Comparable() {
		return nil, fmt.Errorf("key is not comparable")
	}

	oriData := b.data[key]
	b.data[key] = value
	return oriData, nil
}

func (b *pipelineContextDataBucket) QueryData(key interface{}) interface{} {
	b.RLock()
	defer b.RUnlock()
	return b.data[key]
}

func (b *pipelineContextDataBucket) QueryDataWithBindDefault(key interface{},
	defaultValueFunc pipelines.DefaultValueFunc) (interface{}, error) {

	b.Lock()
	defer b.Unlock()

	var value interface{}

	_value, exists := b.data[key]
	if !exists {
		value = defaultValueFunc()
		_, err := b.bindData(key, value)
		if err != nil {
			return nil, err
		}
	} else {
		value = _value
	}

	return value, nil
}

func (b *pipelineContextDataBucket) UnbindData(key interface{}) interface{} {
	b.Lock()
	defer b.Unlock()

	oriData := b.data[key]
	delete(b.data, key)
	return oriData
}
