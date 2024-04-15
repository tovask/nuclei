package core

import (
	"fmt"
	"net/http/cookiejar"
	"sync/atomic"

	"github.com/remeh/sizedwaitgroup"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v3/pkg/output"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/contextargs"
	"github.com/projectdiscovery/nuclei/v3/pkg/scan"
	"github.com/projectdiscovery/nuclei/v3/pkg/workflows"
)

const workflowStepExecutionError = "[%s] Could not execute workflow step: %s\n"

/*
TODO:
 - remove unnecessary fields from ScanContext
 - create a flowcharts about the Execute/ExecuteWithResults and the result's way to output
     what is the purpose of the LogEvent in ScanContext?
     when should the ScanContext.LogEvent be called? or the LogError ...
     maybe in TemplateExecuter.ExecuteWithResults ?
 - restructre workflow logic (subtemplates, matchers)
 - update DESIGN.md
 - bugs:
	 - https://github.com/tovask/nuclei/commit/03718469c47ea6296593865f8d64377fbf3471d7#diff-47ab30eed712e7b31af2c38be7ac02459f075910f108f0ba727dc6ac0706ed77R25
	 - the DNS request executer verifies the input of the whole scan instead of the actual request that can differ => incorrect error: "cannot use IP address as DNS input"
*/

// executeWorkflow runs a workflow on an input and returns true or false
func (e *Engine) executeWorkflow(ctx *scan.ScanContext, w *workflows.Workflow) bool {
	results := &atomic.Bool{}

	// at this point we should be at the start root execution of a workflow tree, hence we create global shared instances
	workflowCookieJar, _ := cookiejar.New(nil)
	ctxArgs := contextargs.New()
	ctxArgs.MetaInput = ctx.Input.MetaInput
	ctxArgs.CookieJar = workflowCookieJar

	// we can know the nesting level only at runtime, so the best we can do here is increase template threads by one unit in case it's equal to 1 to allow
	// at least one subtemplate to go through, which it's idempotent to one in-flight template as the parent one is in an idle state
	templateThreads := w.Options.Options.TemplateThreads
	if templateThreads == 1 {
		templateThreads++
	}
	swg := sizedwaitgroup.New(templateThreads)

	for _, template := range w.Workflows {
		swg.Add()

		func(template *workflows.WorkflowTemplate) {
			defer swg.Done()

			/*
			scanCtx := scan.NewScanContext(ctx.Input)
			inputItem := ctx.Input.Clone()
			*/
			if err := e.runWorkflowStep(template, ctx, results, &swg, w); err != nil {
				gologger.Warning().Msgf(workflowStepExecutionError, template.Template, err)
			}
		}(template)
	}
	swg.Wait()
	return results.Load()
}

// runWorkflowStep runs a workflow step for the workflow. It executes the workflow
// in a recursive manner running all subtemplates and matchers.
func (e *Engine) runWorkflowStep(template *workflows.WorkflowTemplate, ctx *scan.ScanContext, results *atomic.Bool, swg *sizedwaitgroup.SizedWaitGroup, w *workflows.Workflow) error {
	var firstMatched bool
	var err error
	var mainErr error

	//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s:\trunWorkflowStep\n", template.Template)
	//fmt.Print("\t\t\t\t\t\t\t\t\t\t\t\t")
	//fmt.Println(template.Subtemplates)

	if len(template.Matchers) == 0 {
		for _, executer := range template.Executers {
			executer.Options.Progress.AddToTotal(int64(executer.Executer.Requests()))

			// Don't print results with subtemplates, only print results on template.
			if len(template.Subtemplates) > 0 {
				//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s:\tOnResult set from workflow_execute under Subtemplates", template.Template)
				//fmt.Println(template.Subtemplates)
				//fmt.Println(ctx.OnResult)
				//ctx.OnResult = func(result *output.InternalWrappedEvent) {
				err = executer.Executer.ExecuteWithResults(ctx, func(result *output.InternalWrappedEvent) {
					if result.OperatorsResult == nil {
						return
					}
					if len(result.Results) > 0 {
						firstMatched = true
					}

					if result.OperatorsResult != nil && result.OperatorsResult.Extracts != nil {
						for k, v := range result.OperatorsResult.Extracts {
							// normalize items:
							switch len(v) {
							case 0, 1:
								// - key:[item] => key: item
								ctx.Input.Set(k, v[0])
							default:
								// - key:[item_0, ..., item_n] => key0:item_0, keyn:item_n
								for vIdx, vVal := range v {
									normalizedKIdx := fmt.Sprintf("%s%d", k, vIdx)
									ctx.Input.Set(normalizedKIdx, vVal)
								}
								// also add the original name with full slice
								ctx.Input.Set(k, v)
							}
						}
					}
				})
				//fmt.Println(ctx.OnResult)
				// _, err = executer.Executer.ExecuteWithResults(ctx)
			} else {
				var matched bool
				matched, err = executer.Executer.Execute(ctx)
				if matched {
					firstMatched = true
				}
			}
			if err != nil {
				if w.Options.HostErrorsCache != nil {
					w.Options.HostErrorsCache.MarkFailed(ctx.Input.MetaInput.ID(), err)
				}
				if len(template.Executers) == 1 {
					mainErr = err
				} else {
					gologger.Warning().Msgf(workflowStepExecutionError, template.Template, err)
				}
				continue
			}
		}
	}
	if len(template.Subtemplates) == 0 {
		results.CompareAndSwap(false, firstMatched)
	}
	if len(template.Matchers) > 0 {
		for _, executer := range template.Executers {
			executer.Options.Progress.AddToTotal(int64(executer.Executer.Requests()))

			//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s:\tOnResult set from workflow_execute under Matchers\n", template.Template)
			//fmt.Println(template.Matchers[0].Subtemplates[0].Template)
			//fmt.Println(ctx.OnResult)
			// ctx.OnResult = func(event *output.InternalWrappedEvent) {
			err = executer.Executer.ExecuteWithResults(ctx, func(event *output.InternalWrappedEvent) {
				//wrappedTemplate := template.Template
				//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s: OnResult %s %s\n", wrappedTemplate, template.Template, event.InternalEvent["template-id"])
				if event.OperatorsResult == nil {
					return
				}
				//fmt.Println(event.OperatorsResult.Operators.TemplateID)
				//fmt.Println(event.OperatorsResult.Matches)

				if event.OperatorsResult.Extracts != nil {
					for k, v := range event.OperatorsResult.Extracts {
						ctx.Input.Set(k, v)
					}
				}

				for _, matcher := range template.Matchers {
					if !matcher.Match(event.OperatorsResult) {
						continue
					}

					for _, subtemplate := range matcher.Subtemplates {
						swg.Add()

						go func(subtemplate *workflows.WorkflowTemplate) {
							defer swg.Done()

							if err := e.runWorkflowStep(subtemplate, ctx, results, swg, w); err != nil {
								gologger.Warning().Msgf(workflowStepExecutionError, subtemplate.Template, err)
							}
						}(subtemplate)
					}
				}
			})
			//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s:\tExecuteWithResults BEFORE\n", template.Template)
			// _, err := executer.Executer.ExecuteWithResults(ctx)
			//fmt.Printf("\t\t\t\t\t\t\t\t\t\t\t%s:\tExecuteWithResults AFTER\n", template.Template)
			if err != nil {
				if len(template.Executers) == 1 {
					mainErr = err
				} else {
					gologger.Warning().Msgf(workflowStepExecutionError, template.Template, err)
				}
				continue
			}
		}
		return mainErr
	}
	if len(template.Subtemplates) > 0 && firstMatched {
		for _, subtemplate := range template.Subtemplates {
			swg.Add()

			go func(template *workflows.WorkflowTemplate) {
				if err := e.runWorkflowStep(template, ctx, results, swg, w); err != nil {
					gologger.Warning().Msgf(workflowStepExecutionError, template.Template, err)
				}
				swg.Done()
			}(subtemplate)
		}
	}
	return mainErr
}
