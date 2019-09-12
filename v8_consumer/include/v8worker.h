// Copyright (c) 2017 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an "AS IS"
// BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing
// permissions and limitations under the License.

#ifndef V8WORKER_H
#define V8WORKER_H

#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <fstream>
#include <libcouchbase/api3.h>
#include <libcouchbase/couchbase.h>
#include <libplatform/libplatform.h>
#include <list>
#include <map>
#include <regex>
#include <sstream>
#include <string>
#include <thread>
#include <uv.h>
#include <v8-debug.h>
#include <v8.h>

#include "commands.h"
#include "crc32c.h"
#include "function_templates.h"
#include "histogram.h"
#include "inspector_agent.h"
#include "js_exception.h"
#include "log.h"
#include "n1ql.h"
#include "queue.h"
#include "utils.h"

#include "../../gen/flatbuf/header_generated.h"
#include "../../gen/flatbuf/payload_generated.h"
#include "../../gen/flatbuf/response_generated.h"

typedef std::chrono::high_resolution_clock Time;
typedef std::chrono::nanoseconds nsecs;

#define SECS_TO_NS 1000 * 1000 * 1000ULL

// Histogram to capture latency stats. Latency buckets have granularity of 1ms,
// starting from 100us to 10s
#define HIST_FROM 100
#define HIST_TILL 1000 * 1000 * 10
#define HIST_WIDTH 1000

#define NUM_VBUCKETS 1024

extern int64_t timer_context_size;

using atomic_ptr_t = std::shared_ptr<std::atomic<uint64_t>>;
// Used for checkpointing of vbucket seq nos
typedef std::map<int, atomic_ptr_t> vb_seq_map_t;

typedef struct timer_msg_s {
  std::size_t GetSize() const { return timer_entry.length(); }

  std::string timer_entry;
} timer_msg_t;

// Header frame structure for messages from Go world
struct MessageHeader {
  MessageHeader() = default;
  ~MessageHeader() = default;
  MessageHeader(MessageHeader &&other) noexcept
      : event(other.event), opcode(other.opcode), partition(other.partition),
        metadata(std::move(other.metadata)) {}

  MessageHeader &operator=(MessageHeader &&other) noexcept {
    event = other.event;
    opcode = other.opcode;
    partition = other.partition;
    metadata = std::move(other.metadata);
    return *this;
  }
  MessageHeader(const MessageHeader &other) = delete;
  MessageHeader &operator=(const MessageHeader &other) = delete;

  std::size_t GetSize() const {
    return metadata.length() + sizeof(event) + sizeof(opcode) +
           sizeof(partition) + metadata.length();
  }

  uint8_t event{0};
  uint8_t opcode{0};
  int16_t partition{0};
  std::string metadata;
};

// Flatbuffer encoded message from Go world
struct MessagePayload {
  MessagePayload() = default;
  ~MessagePayload() = default;
  MessagePayload(MessagePayload &&other) noexcept
      : header(std::move(other.header)), payload(std::move(other.payload)) {}

  MessagePayload &operator=(MessagePayload &&other) noexcept {
    header = std::move(other.header);
    payload = std::move(other.payload);
    return *this;
  }
  MessagePayload(const MessagePayload &other) = delete;
  MessagePayload &operator=(const MessagePayload &other) = delete;

  std::size_t GetSize() const { return header.length() + payload.length(); }

  std::string header;
  std::string payload;
};

// Struct to contain flatbuffer decoded message from Go world
struct WorkerMessage {
  WorkerMessage() = default;
  ~WorkerMessage() = default;
  WorkerMessage(WorkerMessage &&other) noexcept
      : header(std::move(other.header)), payload(std::move(other.payload)) {}

  WorkerMessage &operator=(WorkerMessage &&other) noexcept {
    header = std::move(other.header);
    payload = std::move(other.payload);
    return *this;
  }
  WorkerMessage(const WorkerMessage &other) = delete;
  WorkerMessage &operator=(const WorkerMessage &other) = delete;

  std::size_t GetSize() const { return header.GetSize() + payload.GetSize(); }

  MessageHeader header;
  MessagePayload payload;
};

typedef struct server_settings_s {
  int checkpoint_interval;
  std::string debugger_port;
  std::string eventing_dir;
  std::string eventing_port;
  std::string eventing_sslport;
  std::string host_addr;
  std::string kv_host_port;
} server_settings_t;

typedef struct handler_config_s {
  std::string app_name;
  long curl_timeout;
  std::string dep_cfg;
  int execution_timeout;
  int lcb_inst_capacity;
  bool skip_lcb_bootstrap;
  int64_t timer_context_size;
  std::vector<std::string> handler_headers;
  std::vector<std::string> handler_footers;
} handler_config_t;

enum RETURN_CODE {
  kSuccess = 0,
  kFailedToCompileJs,
  kNoHandlersDefined,
  kFailedInitBucketHandle,
  kOnUpdateCallFail,
  kOnDeleteCallFail,
  kToLocalFailed
};

class Bucket;
class N1QL;
class ConnectionPool;
class V8Worker;

extern std::atomic<int64_t> bucket_op_exception_count;
extern std::atomic<int64_t> n1ql_op_exception_count;
extern std::atomic<int64_t> timeout_count;
extern std::atomic<int16_t> checkpoint_failure_count;

extern std::atomic<int64_t> on_update_success;
extern std::atomic<int64_t> on_update_failure;
extern std::atomic<int64_t> on_delete_success;
extern std::atomic<int64_t> on_delete_failure;

extern std::atomic<int64_t> timer_create_failure;

extern std::atomic<int64_t> lcb_retry_failure;

extern std::atomic<int64_t> messages_processed_counter;

// DCP or Timer event counter
extern std::atomic<int64_t> dcp_delete_msg_counter;
extern std::atomic<int64_t> dcp_mutation_msg_counter;
extern std::atomic<int64_t> timer_msg_counter;

extern std::atomic<int64_t> enqueued_dcp_delete_msg_counter;
extern std::atomic<int64_t> enqueued_dcp_mutation_msg_counter;
extern std::atomic<int64_t> enqueued_timer_msg_counter;

class V8Worker {
public:
  V8Worker(v8::Platform *platform, handler_config_t *config,
           server_settings_t *settings, const std::string &handler_name,
           const std::string &handler_uuid, const std::string &user_prefix);
  ~V8Worker();

  int V8WorkerLoad(std::string source_s);
  void RouteMessage();
  void TaskDurationWatcher();

  int SendUpdate(std::string value, std::string meta, int vb_no,
                 uint64_t seq_no, std::string doc_type);
  int SendDelete(std::string meta, int vb_no, uint64_t seq_no);
  void SendTimer(std::string callback, std::string timer_ctx);
  std::string CompileHandler(std::string handler);
  CodeVersion IdentifyVersion(std::string handler);

  void StartDebugger();
  void StopDebugger();
  bool DebugExecute(const char *func_name, v8::Local<v8::Value> *args,
                    int args_len);

  void Enqueue(std::unique_ptr<WorkerMessage> worker_msg);

  void AddLcbException(int err_code);
  void ListLcbExceptions(std::map<int, int64_t> &agg_lcb_exceptions);

  void UpdateHistogram(Time::time_point t);

  void GetTimerMessages(std::vector<uv_buf_t> &messages, size_t window_size);

  void GetBucketOpsMessages(std::vector<uv_buf_t> &messages);

  void UpdateVbFilter(int vb_no, uint64_t seq_no);

  uint64_t GetVbFilter(int vb_no);

  void EraseVbFilter(int vb_no);

  void UpdateBucketopsSeqno(int vb_no, uint64_t seq_no);

  uint64_t GetBucketopsSeqno(int vb_no);

  void FilterLock();

  void FilterUnlock();

  int ParseMetadata(const std::string &metadata, int &vb_no, uint64_t &seq_no);
  int ParseMetadataWithAck(const std::string &metadata, int &vb_no,
                           uint64_t &seq_no, int &skip_ack, bool ack_check);

  void SetThreadExitFlag();

  v8::Isolate *GetIsolate() { return isolate_; }
  v8::Persistent<v8::Context> context_;
  v8::Persistent<v8::Context> utility_context_;
  v8::Persistent<v8::Function> on_update_;
  v8::Persistent<v8::Function> on_delete_;

  std::string app_name_;
  std::string script_to_execute_;
  std::string cb_source_bucket_;
  int64_t max_task_duration_;

  server_settings_t *settings_;

  volatile bool shutdown_terminator_;
  static bool debugger_started_;

  uint64_t currently_processed_vb_;
  uint64_t currently_processed_seqno_;
  Time::time_point execute_start_time_;

  std::thread processing_thr_;
  std::thread *terminator_thr_;
  Queue<std::unique_ptr<timer_msg_t>> *timer_queue_;
  Queue<std::unique_ptr<WorkerMessage>> *worker_queue_;

  ConnectionPool *conn_pool_;
  JsException *js_exception_;

  std::mutex lcb_exception_mtx_;
  std::map<int, int64_t> lcb_exceptions_;

  Histogram *histogram_;
  Data data_;

private:
  std::vector<uv_buf_t> BuildResponse(const std::string &payload,
                                      int8_t msg_type, int8_t response_opcode);
  bool ExecuteScript(const v8::Local<v8::String> &script);
  void InitializeUtility(const v8::Isolate::CreateParams &params);

  std::string connstr_;
  std::string meta_connstr_;
  std::string src_path_;

  vb_seq_map_t vb_seq_;

  std::vector<std::vector<uint64_t>> vbfilter_map_;
  std::mutex vbfilter_lock_;
  std::vector<uint64_t> processed_bucketops_;
  std::mutex bucketops_lock_;
  std::list<Bucket *> bucket_handles_;
  N1QL *n1ql_handle_;
  v8::Isolate *isolate_;
  v8::Isolate *utility_isolate_;
  v8::Platform *platform_;
  inspector::Agent *agent_;
  std::string handler_name_;
  std::string handler_uuid_;
  std::string user_prefix_;
  std::atomic<bool> thread_exit_cond_;
  const std::vector<std::string> exception_type_names_;
};

const char *GetUsername(void *cookie, const char *host, const char *port,
                        const char *bucket);
const char *GetPassword(void *cookie, const char *host, const char *port,
                        const char *bucket);
const char *GetUsernameCached(void *cookie, const char *host, const char *port,
                              const char *bucket);
const char *GetPasswordCached(void *cookie, const char *host, const char *port,
                              const char *bucket);
#endif
