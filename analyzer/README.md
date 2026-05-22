## You can analyze your generated beats report in a few quick simple steps.

Say you want to analyze reports generated for /Users/admin/ws/golang/gitea
```
pip3 install -r requirements.txt
python analyze_report.py /Users/admin/ws/golang/gitea/.beats/report.html          # rich terminal output
python analyze_report.py /Users/admin/ws/golang/gitea/.beats/report.html --json   # structured JSON
python analyze_report.py /Users/admin/ws/golang/gitea/.beats/report.html --top 20 # show top 20 per table

```

## Remember

|                      | **High Call Cohesion** | **Low Call Cohesion** |
|----------------------|-------------------------|------------------------|
| **High Import Cohesion** | **Tight domain-local pattern**<br>Shares both package context and call vocabulary. Highly actionable. | **Domain-cohesive, structurally diverse**<br>Shared package domain, but divergent call patterns. May benefit from splitting. |
| **Low Import Cohesion** | **Cross-cutting structural pattern**<br>Different domains, but similar structural roles (e.g., cron registration, adapters). | **Likely noise**<br>Coincidental structural similarity rather than a meaningful convention. Treat with skepticism. |

## Sample

A sample output looks like so

```
Found 509 clusters with 1367 member functions
╭─────────────────────────────────────────────────────────────────────────────────── beats report analysis ────────────────────────────────────────────────────────────────────────────────────╮
│ beats analyze  gitea                                                                                                                                                                         │
│ 2026-05-21 09:48:55                                                                                                                                                                          │
│                                                                                                                                                                                              │
│ Clusters  509   Functions  1367   Mean Import Coh.  0.82   Mean Call Coh.  0.59                                                                                                              │
╰──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
               Quadrant Distribution                
                                                    
  Quadrant   Count   Share   Description            
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  HH           228     45%   HH tight domain-local  
  LH            31      6%   LH cross-cutting       
  HL           203     40%   HL domain-cohesive     
  LL            47      9%   LL noise               
                                                    
Combined Coherence Distribution 
                                
  Band        Clusters   Share  
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  ≥0.90            120     24%  
  0.70–0.89        136     27%  
  0.50–0.69        198     39%  
  <0.50             55     11%  
                                
                                                  Top 10 Clusters by Size                                                   
                                                                                                                            
  Type   Hash                 Size   ImportCoh   CallCoh   CycloP95   Packages                Top Imports                   
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  HL     18cf1151e6156b6a       58        1.00      0.00        1.0   v1_10, v1_11            xorm                          
  HH     cdec1a010041a46a       17        1.00      1.00        1.0   actions, activities     container                     
  HL     18cf1151e6156b6a-2     17        0.90      0.00        1.0   v1_10, v1_14            timeutil, xorm, pull +2       
  LL     92a725126f56c075       16        0.59      0.00        1.0   cron                    user, context, actions +2     
  HL     42b90e9afc06d444-3     13        0.79      0.12        1.0   auth, conda             strings, v1, slices           
  LH     2c9353dce3c648a6-4     10        0.54      1.00        1.0   conan, mailer           fmt, repo, setting +2         
  HL     72424455eb8d01bf        9        1.00      0.00        1.0   migration               context                       
  HH     90b06a93c3c70c2b        8        0.77      1.00        5.6   v1_12, v1_14            fmt, xorm, timeutil +1        
  HH     56cb6182920b9db1        8        0.71      1.00        2.0   activities, container   db, context, organization +2  
  HL     7325a0faa4b43727        8        1.00      0.00        1.0   cmd                     context                       
                                                                                                                            
                               Top 10 Clusters by Combined Coherence                               
                                                                                                   
  Type   Hash                 Size   ImportCoh   CallCoh   Combined   Packages                     
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  HH     cdec1a010041a46a       17        1.00      1.00       1.00   actions, activities, issues  
  HH     561da6bf80a4bec2        5        1.00      1.00       1.00   webhook                      
  HH     d8239fa84d16bb0f        5        1.00      1.00       1.00   actions                      
  HH     45c15e18bfbf0e07        5        1.00      1.00       1.00   webhook                      
  HH     4c6a6bcc46db8282        4        1.00      1.00       1.00   activities, issues           
  HH     af168bed8e60c53a        4        1.00      1.00       1.00   v1_11, v1_15, v1_21          
  HH     d2971c27c427cc7a        4        1.00      1.00       1.00   webhook                      
  HH     e7a07423a4a7f613        4        1.00      1.00       1.00   repo                         
  HH     b67ea2bf28eb5451        4        1.00      1.00       1.00   v1_20                        
  HH     e6dcda91f87c7064-1      4        1.00      1.00       1.00   meilisearch                  
                                                                                                   
                    Generated Code Candidates  (Cyclo P95 ≥ 50, 1 clusters)                     
                                                                                                
  Hash               Size   CycloP95   Packages   Sample members                                
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  9defb54228a02834      2       64.0   repo       CreateBranchProtection, EditBranchProtection  
                                                                                                
              Cross-Package Structural Patterns  (215 clusters)              
                                                                             
  Type   Hash                 Size   Combined   Packages                     
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  HH     cdec1a010041a46a       17       1.00   actions, activities, issues  
  HH     4c6a6bcc46db8282        4       1.00   activities, issues           
  HH     af168bed8e60c53a        4       1.00   v1_11, v1_15, v1_21, v1_8    
  HH     bf54d4e828e7796f        3       1.00   options, public, templates   
  HH     12fe0b6683748ae8        3       1.00   migration, migrations        
  HH     2c9353dce3c648a6-2      3       1.00   context, htmlutil, markup    
  HH     30843cef1b2474a1        3       1.00   issues, user                 
  HH     de11e56e29f9d7a0        2       1.00   feed, uinotification         
  HH     bf54d4e828e7796f-1      2       1.00   auth, hash                   
  HH     64f951bf1d42f953        2       1.00   v1_11, v1_14                 
                                                                             
            Top Packages by Clustered Members            
                                                         
  Package      Members   Bar                             
 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 
  repo             136   ██████████████████████████████  
  issues            97   █████████████████████           
  actions           81   █████████████████               
  webhook           66   ██████████████                  
  git               58   ████████████                    
  user              40   ████████                        
  migrations        35   ███████                         
  gitrepo           27   █████                           
  cron              27   █████                           
  activities        26   █████                           
  repository        26   █████                           
  cmd               22   ████                            
  setting           21   ████                            
  container         21   ████                            
  conan             20   ████    

```